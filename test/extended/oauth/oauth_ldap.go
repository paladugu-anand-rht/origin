package oauth

import (
	"encoding/base64"
	"fmt"
	"time"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	restclient "k8s.io/client-go/rest"

	configv1 "github.com/openshift/api/config/v1"
	osinv1 "github.com/openshift/api/osin/v1"
	userv1client "github.com/openshift/client-go/user/clientset/versioned/typed/user/v1"
	testutil "github.com/openshift/origin/test/extended/util"
	oauthutil "github.com/openshift/origin/test/extended/util/oauthserver"
)

var _ = g.Describe("[Suite:openshift/oauth] LDAP IDP", func() {
	defer g.GinkgoRecover()
	var (
		oc = testutil.NewCLI("oauth-ldap-idp", testutil.KubeConfigPath())

		bindDN         = "cn=Manager,dc=example,dc=com"
		bindPassword   = "admin"
		ldapScheme     = "ldap://"
		ldapPort       = ":389"
		caPath         = "/var/oauth/configobjects/ldapca/ca.crt"
		searchDN       = "ou=people,ou=rfc2307,dc=example,dc=com"
		searchAttr     = "cn"
		searchScope    = "one"
		userName       = "Person1"
		goodPass       = "foobar"
		badPass        = "baz"
		providerName   = "ldapidp"
		myUserDNBase64 = base64.RawURLEncoding.EncodeToString([]byte(searchAttr + "=" + userName + "," + searchDN))
		myUserName     = "person1smith"
		myEmail        = "person1smith@example.com"
	)

	adminConfig := oc.AdminConfig()

	g.It("should authenticate against an ldap server", func() {
		// Clean up mapped identity and user.
		defer userv1client.NewForConfigOrDie(oc.AdminConfig()).Identities().Delete(fmt.Sprintf("%s:%s", providerName, myUserDNBase64), nil)
		defer userv1client.NewForConfigOrDie(oc.AdminConfig()).Users().Delete(userName, nil)

		g.By("setting up an OpenLDAP server")
		ldapService, ldapCA, err := testutil.CreateLDAPTestServer(oc)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("deploying an oauth server")
		caConfigMap := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ldapca",
			},
			Data: map[string]string{
				"ca.crt": string(ldapCA),
			},
		}

		// Configure IDP
		ldapProvider, err := oauthutil.GetRawExtensionForOsinProvider(
			&osinv1.LDAPPasswordIdentityProvider{
				URL:    ldapScheme + ldapService + ldapPort + fmt.Sprintf("/%s?%s?%s", searchDN, searchAttr, searchScope),
				BindDN: bindDN,
				BindPassword: configv1.StringSource{StringSourceSpec: configv1.StringSourceSpec{
					Value: bindPassword,
				}},
				Insecure: false,
				CA:       caPath,
				Attributes: osinv1.LDAPAttributeMapping{
					ID:                []string{"dn"},
					PreferredUsername: []string{"cn"},
					Name:              []string{"displayName"},
					Email:             []string{"mail"},
				},
			},
		)
		o.Expect(err).ToNot(o.HaveOccurred())

		// Deploy an OAuth server
		tokenOpts, _, err := oauthutil.DeployOAuthServer(oc, []osinv1.IdentityProvider{
			{
				Name:            providerName,
				UseAsChallenger: true,
				UseAsLogin:      true,
				MappingMethod:   "claim",
				Provider:        *ldapProvider,
			},
		}, []corev1.ConfigMap{caConfigMap}, nil)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("configuring LDAP users")
		volumeMounts, volumes := testutil.LDAPClientMounts()
		_, errs := testutil.RunOneShotCommandPod(oc, "oneshot-ldappasswd", testutil.OpenLDAPTestImage, fmt.Sprintf("ldappasswd -x -H ldap://%s -Z -D %s -w %s -s %s cn=%s,%s -vvv", ldapService, bindDN, bindPassword, goodPass, userName, searchDN), volumeMounts, volumes, nil, 5*time.Minute)
		o.Expect(errs).To(o.BeEmpty())

		g.By("ensuring that you cannot authenticate with a bad password")
		_, err = tokenOpts(userName, badPass).RequestToken()
		o.Expect(err).Should(o.MatchError("challenger chose not to retry the request"))

		g.By("authenticating with LDAP user")
		person1Token, err := tokenOpts(userName, goodPass).RequestToken()
		o.Expect(err).NotTo(o.HaveOccurred())

		// Make sure we can use the token, and it represents who we expect
		userConfig := restclient.AnonymousClientConfig(adminConfig)
		userConfig.BearerToken = person1Token

		// Confirm user name
		user, err := userv1client.NewForConfigOrDie(userConfig).Users().Get("~", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(user.Name).Should(o.BeEquivalentTo(userName))
		o.Expect(user.Identities).Should(o.HaveLen(1))

		adminClient := userv1client.NewForConfigOrDie(oc.AdminConfig())
		// Make sure the identity got created and contained the mapped attributes
		identity, err := adminClient.Identities().Get(fmt.Sprintf("%s:%s", providerName, myUserDNBase64), metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(identity.User.Name).Should(o.BeEquivalentTo(user.Name))
		o.Expect(identity.ProviderName + ":" + identity.ProviderUserName).Should(o.BeEquivalentTo(user.Identities[0]))
		o.Expect(identity.ProviderUserName).Should(o.BeEquivalentTo(myUserDNBase64))
		o.Expect(identity.Extra["email"]).Should(o.BeEquivalentTo(myEmail))
		o.Expect(identity.Extra["preferred_username"]).Should(o.BeEquivalentTo(userName))
		o.Expect(identity.Extra["name"]).Should(o.BeEquivalentTo(myUserName))
	})
})

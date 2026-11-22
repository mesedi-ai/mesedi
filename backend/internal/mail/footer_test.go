package mail

import (
	"regexp"
	"strings"
	"testing"
)

// The emails name only Mesedi and link to the terms and privacy pages,
// where the legal provider is stated. The provider's name stays out of
// the email body.
func TestEmailFootersNameMesediAndLinkTheLegalPages(t *testing.T) {
	bodies := map[string]string{
		"welcome text":    welcomeText(WelcomeInput{ProjectName: "p"}),
		"welcome html":    welcomeHTML(WelcomeInput{ProjectName: "p"}),
		"suspension text": suspensionWarningText(SuspensionWarningInput{ProjectName: "p"}),
		"suspension html": suspensionWarningHTML(SuspensionWarningInput{ProjectName: "p"}),
	}
	for name, body := range bodies {
		if strings.Contains(body, "Verdifax") || strings.Contains(body, "d/b/a") {
			t.Errorf("%s names the legal provider; only the terms and privacy pages should", name)
		}
		for _, link := range []string{"https://mesedi.ai/terms", "https://mesedi.ai/privacy"} {
			if !strings.Contains(body, link) {
				t.Errorf("%s does not link %s", name, link)
			}
		}
	}
}

// Every colour in the HTML emails keeps its "#". A clean-up once took
// "#0a0a0a" to "a0a0a" and "#666" to nothing, and the suspension email
// lost its dark background with no error anywhere.
func TestEmailColoursKeepTheirHash(t *testing.T) {
	broken := regexp.MustCompile(`(?:background|color):(?:[0-9a-f]{3,6})?[;"]|solid [0-9a-f]{3,6}[;"]`)
	for name, body := range map[string]string{
		"welcome html":    welcomeHTML(WelcomeInput{ProjectName: "p"}),
		"suspension html": suspensionWarningHTML(SuspensionWarningInput{ProjectName: "p"}),
	} {
		if m := broken.FindAllString(body, -1); len(m) > 0 {
			t.Errorf("%s has colours without a #: %v", name, m)
		}
	}
}

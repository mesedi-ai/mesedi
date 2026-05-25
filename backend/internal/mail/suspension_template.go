// Suspension-warning email templates (#172).
//
// Sent 24h after an abuse signal first fires. Customer gets another
// 24h to respond before auto-suspension. Voice is calm, direct, and
// non-accusatory: the goal is to recover the customer if it's a
// misconfiguration, not to punish if it's deliberate abuse.

package mail

import (
	"fmt"
	"html"
)

func suspensionWarningText(in SuspensionWarningInput) string {
	return fmt.Sprintf(`Action required on your Mesedi project.

We detected sustained abuse signal on your project "%s":

  Kind: %s
  First seen: %s UTC

Per our Terms of Service, accounts that trigger abuse signal receive
24 hours of notice before automatic suspension. You have until
~24 hours from now to address the issue.

Most common causes (and fixes):

  rate_limit_sustained   An agent or client is hammering the API
                         without backoff. Check for an infinite
                         loop, a retry storm, or a stuck client
                         polling without a real cap.

  key_leak               The same API key is being seen from many
                         different IP addresses. Rotate the key
                         immediately if it might have been
                         committed to a public repo or shared.

  oversized_payload      Repeated requests with bodies > 1 MB.
                         Usually a debug log that's been wired
                         into an event payload. Cap the payload
                         size client-side.

  suspicious_signup      Many signups from a single IP. If this is
                         a CI runner or shared NAT, reply to this
                         email and we'll whitelist it.

If you believe this is in error, reply to this email and we'll
take a look before suspension fires.

View your project dashboard:
%s

Mesedi
Verdifax, LLC d/b/a Mesedi
`,
		in.ProjectName,
		in.SignalKind,
		in.DetectedAt.UTC().Format("2006-01-02 15:04"),
		in.DashboardURL,
	)
}

func suspensionWarningHTML(in SuspensionWarningInput) string {
	return fmt.Sprintf(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>Action required on your Mesedi project</title>
</head>
<body style="margin:0;padding:0;background:#0a0a0a;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;color:#e5e5e5;">
<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" border="0" style="background:#0a0a0a;">
  <tr>
    <td align="center" style="padding:32px 16px;">
      <table role="presentation" width="560" cellpadding="0" cellspacing="0" border="0" style="max-width:560px;width:100%%;">
        <tr>
          <td style="padding-bottom:24px;">
            <h1 style="margin:0;font-size:24px;font-weight:600;color:#fafafa;letter-spacing:-0.01em;">Action required on your Mesedi project.</h1>
          </td>
        </tr>
        <tr>
          <td style="font-size:15px;line-height:1.6;color:#b5b5b5;padding-bottom:20px;">
            We detected sustained abuse signal on your project
            <strong style="color:#fafafa;">%s</strong>.
          </td>
        </tr>
        <tr>
          <td style="padding-bottom:20px;">
            <table cellpadding="0" cellspacing="0" border="0" style="background:#1a1a1a;border:1px solid #2a2a2a;border-radius:6px;padding:14px;width:100%%;">
              <tr>
                <td style="font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px;line-height:1.7;color:#fafafa;">
                  Kind: %s<br>
                  First seen: %s UTC
                </td>
              </tr>
            </table>
          </td>
        </tr>
        <tr>
          <td style="font-size:15px;line-height:1.6;color:#b5b5b5;padding-bottom:20px;">
            Per our Terms of Service, accounts that trigger abuse signal receive
            <strong style="color:#fafafa;">24 hours of notice</strong> before automatic
            suspension. You have until ~24 hours from now to address the issue or
            reply to this email if you believe this is in error.
          </td>
        </tr>
        <tr>
          <td style="padding-bottom:24px;">
            <a href="%s" style="display:inline-block;background:#f97316;color:#0a0a0a;text-decoration:none;font-weight:600;padding:12px 18px;border-radius:6px;font-size:14px;">Open dashboard &rarr;</a>
          </td>
        </tr>
        <tr>
          <td style="font-size:15px;line-height:1.6;color:#b5b5b5;padding:20px 0 0;border-top:1px solid #2a2a2a;">
            Most common causes: a client without backoff (rate_limit_sustained),
            an API key committed to a public repo (key_leak), a debug log
            wired into the event payload (oversized_payload), or many signups
            from a shared NAT or CI runner (suspicious_signup).
          </td>
        </tr>
        <tr>
          <td style="font-size:15px;line-height:1.6;color:#b5b5b5;padding:16px 0;">
            Questions or disputes: reply directly to this email.
          </td>
        </tr>
        <tr>
          <td style="font-size:12px;color:#666;padding:24px 0 0;border-top:1px solid #2a2a2a;">
            Mesedi &middot; Verdifax, LLC d/b/a Mesedi
          </td>
        </tr>
      </table>
    </td>
  </tr>
</table>
</body>
</html>`,
		html.EscapeString(in.ProjectName),
		html.EscapeString(in.SignalKind),
		html.EscapeString(in.DetectedAt.UTC().Format("2006-01-02 15:04")),
		html.EscapeString(in.DashboardURL),
	)
}

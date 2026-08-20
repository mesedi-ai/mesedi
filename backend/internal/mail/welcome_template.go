// Welcome email templates.
//
// HTML and plain-text versions of the welcome email rendered after a
// successful signup. Both versions are kept in sync by hand: the
// transactional volume is low enough that a templating engine adds
// more complexity than it pays for.
//
// Voice and content mirror /docs/quickstart so the reader gets the
// same five steps whether they click through or read the email.

package mail

import (
	"fmt"
	"html"
)

// welcomeVerifyTextBlock returns the plain-text "verify your email"
// preamble if VerifyURL is populated, otherwise an empty string. The
// block leads the email (above the quickstart) because the recipient
// MUST click it before the dashboard unlocks.
func welcomeVerifyTextBlock(in WelcomeInput) string {
	if in.VerifyURL == "" {
		return ""
	}
	return fmt.Sprintf(`Before you can open the dashboard, we need to
confirm this email address. Click the link below within 24 hours:

  %s

(Single-use. If it expires you can request a fresh one from the
"check your email" screen in the dashboard.)

---

`, in.VerifyURL)
}

func welcomeText(in WelcomeInput) string {
	return fmt.Sprintf(`Welcome to Mesedi.

%sYour project "%s" is live. The API key prefix is %s (the raw key was
shown to you once during signup; we don't store it).

Five minutes to your first observed execution:

1. Install the SDK.

   Python:
     pip install mesedi

   TypeScript:
     npm install mesedi

2. Wrap your agent.

   Python:
     import mesedi
     mesedi.configure(api_key="mesedi_sk_...")

     @mesedi.wrap
     def run_my_agent(query: str) -> str:
         return "answer"

     run_my_agent("hello")

   TypeScript:
     import { configure, wrap } from "mesedi";
     configure({ apiKey: process.env.MESEDI_API_KEY! });

     const runMyAgent = wrap(async (query) => "answer");
     await runMyAgent("hello");

3. Open the dashboard.

   %s

   Your first execution lands within a few seconds.

The full quickstart is at %s, including the LangChain, LangGraph,
CrewAI, OpenAI Agents SDK, and Vercel AI SDK adapters.

Questions, corrections, or stuck somewhere? Reply to this email.

Mesedi
Verdifax, LLC d/b/a Mesedi
`,
		welcomeVerifyTextBlock(in),
		in.ProjectName,
		in.APIKeyPrefix,
		in.DashboardURL,
		in.DocsURL,
	)
}

func welcomeHTML(in WelcomeInput) string {
	// Hand-rolled HTML email. Inline styles only; many clients strip
	// <style> blocks. Width capped at 560px which renders well on
	// both desktop and mobile clients without media queries.
	return fmt.Sprintf(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>Welcome to Mesedi</title>
</head>
<body style="margin:0;padding:0;background:#0a0a0a;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;color:#e5e5e5;">
<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" border="0" style="background:#0a0a0a;">
  <tr>
    <td align="center" style="padding:32px 16px;">
      <table role="presentation" width="560" cellpadding="0" cellspacing="0" border="0" style="max-width:560px;width:100%%;">
        <tr>
          <td style="padding-bottom:24px;">
            <h1 style="margin:0;font-size:28px;font-weight:600;color:#fafafa;letter-spacing:-0.01em;">Welcome to Mesedi.</h1>
          </td>
        </tr>
        %s
        <tr>
          <td style="font-size:15px;line-height:1.6;color:#b5b5b5;padding-bottom:24px;">
            Your project <strong style="color:#fafafa;">%s</strong> is live. The API key prefix is
            <code style="background:#1a1a1a;padding:2px 6px;border-radius:4px;color:#fafafa;">%s</code>
            (the raw key was shown to you once during signup; we don&rsquo;t store it).
          </td>
        </tr>

        <tr>
          <td style="font-size:13px;text-transform:uppercase;letter-spacing:0.08em;color:#f97316;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;padding-bottom:8px;">
            Five minutes to your first execution
          </td>
        </tr>

        <tr><td style="padding:12px 0;font-size:15px;color:#fafafa;font-weight:600;">1. Install the SDK</td></tr>
        <tr>
          <td>
            <pre style="background:#1a1a1a;border:1px solid #2a2a2a;border-radius:6px;padding:14px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px;line-height:1.5;color:#fafafa;overflow-x:auto;margin:0;">pip install mesedi          # Python
npm install mesedi          # TypeScript / Node</pre>
          </td>
        </tr>

        <tr><td style="padding:18px 0 8px;font-size:15px;color:#fafafa;font-weight:600;">2. Wrap your agent</td></tr>
        <tr>
          <td>
            <pre style="background:#1a1a1a;border:1px solid #2a2a2a;border-radius:6px;padding:14px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px;line-height:1.5;color:#fafafa;overflow-x:auto;margin:0;">import mesedi
mesedi.configure(api_key="mesedi_sk_...")

@mesedi.wrap
def run_my_agent(query: str) -> str:
    return "answer"

run_my_agent("hello")</pre>
          </td>
        </tr>

        <tr><td style="padding:18px 0 8px;font-size:15px;color:#fafafa;font-weight:600;">3. Open the dashboard</td></tr>
        <tr>
          <td style="font-size:15px;line-height:1.6;color:#b5b5b5;padding-bottom:16px;">
            Your first execution lands within a few seconds.
          </td>
        </tr>
        <tr>
          <td style="padding-bottom:24px;">
            <a href="%s" style="display:inline-block;background:#f97316;color:#0a0a0a;text-decoration:none;font-weight:600;padding:12px 18px;border-radius:6px;font-size:14px;">Open dashboard &rarr;</a>
          </td>
        </tr>

        <tr>
          <td style="font-size:15px;line-height:1.6;color:#b5b5b5;padding:24px 0 0;border-top:1px solid #2a2a2a;">
            The full quickstart is at
            <a href="%s" style="color:#fafafa;">app.mesedi.ai/docs/quickstart</a>,
            including the LangChain, LangGraph, CrewAI, OpenAI Agents SDK, and Vercel AI SDK adapters.
          </td>
        </tr>
        <tr>
          <td style="font-size:15px;line-height:1.6;color:#b5b5b5;padding:16px 0;">
            Questions, corrections, or stuck somewhere? Reply to this email.
          </td>
        </tr>

        <tr>
          <td style="font-size:12px;color:#6b6b6b;padding:24px 0 0;border-top:1px solid #2a2a2a;">
            Mesedi &middot; Verdifax, LLC d/b/a Mesedi
          </td>
        </tr>
      </table>
    </td>
  </tr>
</table>
</body>
</html>`,
		welcomeVerifyHTMLBlock(in),
		html.EscapeString(in.ProjectName),
		html.EscapeString(in.APIKeyPrefix),
		html.EscapeString(in.DashboardURL),
		html.EscapeString(in.DocsURL),
	)
}

// welcomeVerifyHTMLBlock returns the styled HTML "verify your email"
// card if VerifyURL is populated, otherwise an empty string. Empty
// string renders as nothing in the table — the surrounding %s slot
// just collapses.
func welcomeVerifyHTMLBlock(in WelcomeInput) string {
	if in.VerifyURL == "" {
		return ""
	}
	return fmt.Sprintf(`<tr>
  <td style="background:#1f1505;border:1px solid #f97316;border-radius:8px;padding:18px 18px 22px;margin-bottom:24px;font-size:14px;line-height:1.55;color:#fde8c8;">
    <div style="font-size:11px;text-transform:uppercase;letter-spacing:0.08em;color:#f97316;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;padding-bottom:6px;">Step 0 &middot; Verify this email</div>
    Before you can open the dashboard, click the link below within 24 hours.
    <div style="padding-top:14px;">
      <a href="%s" style="display:inline-block;background:#f97316;color:#0a0a0a;text-decoration:none;padding:10px 18px;border-radius:6px;font-weight:600;font-size:14px;">Verify my email &rarr;</a>
    </div>
    <div style="font-size:12px;color:#b5b5b5;padding-top:12px;">Single-use. If the link expires you can ask for a fresh one from the dashboard&rsquo;s &ldquo;check your email&rdquo; screen.</div>
  </td>
</tr>
<tr><td style="height:24px;line-height:24px;font-size:0;">&nbsp;</td></tr>`,
		html.EscapeString(in.VerifyURL))
}

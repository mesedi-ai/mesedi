// Package outbound holds one test: every outside host the backend's code
// names must be declared in it, with who that host belongs to.
//
// Why: the backend sent a failure group's sample events, which can include
// captured prompts and responses, to the Anthropic Messages API whenever a
// user asked for an AI analysis, and Anthropic was missing from the public
// sub-processor list at mesedi.ai/privacy (outside review finding M77,
// 2026-11-15). A new integration now fails this test until it is declared,
// and declaring a sub-processor names the privacy page entry it needs.
//
// The website's own outside hosts are checked by mesedi-web
// tools/check-public-claims.py (outbound_parties). Neither check sees a
// host that arrives only through an environment variable or inside a
// vendor SDK (Stripe, Verdifax); those stay a reader's job.
package outbound

// Every destructive handler must write an audit event.
//
// WHY THIS TEST EXISTS
// On 2026-08-27, 886 failure_groups were deleted from production and
// nothing recorded it. The endpoint responsible, HandleAdminResetFailureGroups,
// wrote a log line and no audit row. Fly's log retention window then
// closed, and by the time the deletion was noticed there was no way to
// determine which project had been reset, by whom, or when.
//
// The deletion was only discovered at all because an R2 backup was
// restored and its row counts compared against live. That is a lucky
// way to find out.
//
// A sweep at the time found EIGHT destructive handlers with no audit
// call, including HandleAdminDeleteProject, which can remove an entire
// customer project. All eight now audit. This test is what stops a
// ninth from appearing.
//
// WHY A SOURCE-LEVEL TEST RATHER THAN A BEHAVIOURAL ONE
// A behavioural test would need a live store, an authenticated request
// and a real deletion per handler, which is a lot of machinery to
// assert one line. It would also only cover handlers someone
// remembered to write a test for, and the failure being guarded
// against is precisely the one nobody remembered. Reading the source
// catches handlers that do not exist yet, which is the whole point.
//
// The check is deliberately crude and deliberately noisy. If it fires
// on something genuinely non-destructive, add it to the allowlist
// below WITH A REASON. Do not delete the test.

package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// destructiveName matches handler names that suggest data removal.
// Over-matching is fine and expected: a false positive costs one line
// in the allowlist, a false negative costs an unattributable deletion.
var destructiveName = regexp.MustCompile(
	`^Handle\w*(Delete|Reset|Purge|Revoke|Wipe|Remove)\w*$`,
)

// callsStore matches an actual mutation through the store layer. A
// handler whose name sounds destructive but never reaches the store
// (a preview, a dry-run, a search over deleted things) is not
// something to audit.
var mutatesStore = regexp.MustCompile(
	`\.Store\.(Delete|Reset|Purge|Revoke|Remove|Wipe|Close|Suspend)\w*\(`,
)

// notDestructive lists handlers whose names trip the pattern but which
// do not remove anything. Every entry needs a reason, because "add it
// to the allowlist" is exactly how this test would be quietly defeated.
var notDestructive = map[string]string{
	"HandleAdminSearchClosedProjectAudit": "read-only search over audit rows for already-closed projects",
}

func TestEveryDestructiveHandlerWritesAnAuditEvent(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	// Match a whole handler body: from its func line to the next
	// line that is exactly "}" at column zero.
	handlerRe := regexp.MustCompile(
		`(?s)\nfunc \(h \*Handlers\) (Handle\w+)\([^)]*\)[^{]*\{.*?\n\}\n`,
	)

	var offenders []string
	checked := 0

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range handlerRe.FindAllStringSubmatch(string(src), -1) {
			body, handler := m[0], m[1]

			if !destructiveName.MatchString(handler) {
				continue
			}
			if reason, ok := notDestructive[handler]; ok {
				if reason == "" {
					t.Errorf("%s is allowlisted with no reason", handler)
				}
				continue
			}
			if !mutatesStore.MatchString(body) {
				// Destructive-sounding but never reaches the store.
				continue
			}

			checked++
			audits := strings.Contains(body, "recordAuditEvent(") ||
				strings.Contains(body, "recordAuditEventForProject(")
			if !audits {
				offenders = append(offenders, handler+"  ("+name+")")
			}
		}
	}

	if checked == 0 {
		// The regex silently matching nothing would make this test
		// pass forever while checking nothing, which is the same
		// class of bug it exists to prevent.
		t.Fatal("matched zero destructive handlers; the detection " +
			"regexes are broken and this test is not actually checking anything")
	}
	t.Logf("checked %d destructive handlers", checked)

	if len(offenders) > 0 {
		t.Errorf(
			"%d destructive handler(s) delete data without writing an audit event:\n  %s\n\n"+
				"Add h.recordAuditEvent(r, ...) for customer-authenticated routes, or\n"+
				"h.recordAuditEventForProject(ctx, projectID, AuditActorPlatformAdmin, ...)\n"+
				"for admin routes, which have no project in request context and where\n"+
				"recordAuditEvent would silently no-op.\n\n"+
				"This matters beyond tidiness: the Privacy Policy promises customers an\n"+
				"audit log of administrative actions retained for seven years. A log line\n"+
				"is not that. Fly's retention window closes and the record is gone.",
			len(offenders), strings.Join(offenders, "\n  "),
		)
	}
}

// TestAdminHandlersDoNotUseRequestScopedAudit guards the specific trap
// that would make the fix above useless.
//
// recordAuditEvent reads the project id from request context and
// SILENTLY RETURNS when it is absent. Admin endpoints authenticate
// with the admin token and carry no project context, so calling it
// there compiles, runs, writes nothing, and looks correct in review.
// That is the same shape as the bug being fixed: a component reporting
// success while doing nothing.
func TestAdminHandlersDoNotUseRequestScopedAudit(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("admin.go")
	if err != nil {
		t.Fatalf("read admin.go: %v", err)
	}

	handlerRe := regexp.MustCompile(
		`(?s)\nfunc \(h \*Handlers\) (HandleAdmin\w+)\([^)]*\)[^{]*\{.*?\n\}\n`,
	)
	// recordAuditEvent( but NOT recordAuditEventForProject(
	badCall := regexp.MustCompile(`recordAuditEvent\(`)

	for _, m := range handlerRe.FindAllStringSubmatch(string(src), -1) {
		body, handler := m[0], m[1]
		stripped := strings.ReplaceAll(body, "recordAuditEventForProject(", "")
		if badCall.MatchString(stripped) {
			t.Errorf(
				"%s uses recordAuditEvent, which reads the project id from request "+
					"context and silently no-ops when it is missing. Admin routes have "+
					"no project context, so this writes nothing. Use "+
					"recordAuditEventForProject(ctx, projectID, AuditActorPlatformAdmin, ...).",
				handler,
			)
		}
	}
}

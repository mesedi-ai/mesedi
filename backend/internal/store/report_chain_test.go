package store

// Report chain storage (migration 067) on SQLite and, when
// MESEDI_TEST_POSTGRES_DSN is set, on Postgres.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestReportChainStore(t *testing.T) {
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			sfx := fmt.Sprintf("%d", time.Now().UnixNano())
			project, exec := "proj-rc-"+sfx, "exec-rc-"+sfx
			rows := []ReportChainRow{
				{EventID: exec + "-1", ExecutionID: exec, ProjectID: project, PrevHash: "g", Hash: "a", PayloadSHA256: "p1"},
				{EventID: exec + "-2", ExecutionID: exec, ProjectID: project, PrevHash: "a", Hash: "b", PayloadSHA256: "p2"},
			}
			if err := c.st.SaveReportChainRows(ctx, rows); err != nil {
				t.Fatal(err)
			}
			// A retried batch stores nothing twice and changes nothing.
			again := rows[0]
			again.Hash = "changed"
			if err := c.st.SaveReportChainRows(ctx, []ReportChainRow{again}); err != nil {
				t.Fatal(err)
			}
			got, err := c.st.ListReportChainRows(ctx, exec)
			if err != nil || len(got) != 2 || got[0].Hash != "a" || got[1].PrevHash != "a" {
				t.Fatalf("rows = %+v, %v", got, err)
			}
			if signed, err := c.st.ExecutionHasSignedReports(ctx, exec); err != nil || signed {
				t.Fatalf("unsigned run reported signed: %v %v", signed, err)
			}
			signedRow := ReportChainRow{EventID: exec + "-3", ExecutionID: exec, ProjectID: project,
				PrevHash: "b", Hash: "c", PayloadSHA256: "p3", KeyID: "k1", Signature: "sig"}
			if err := c.st.SaveReportChainRows(ctx, []ReportChainRow{signedRow}); err != nil {
				t.Fatal(err)
			}
			if signed, err := c.st.ExecutionHasSignedReports(ctx, exec); err != nil || !signed {
				t.Fatalf("signed run not reported signed: %v %v", signed, err)
			}

			if err := c.st.InsertReportSigningKey(ctx, project, "k1", "cHViMQ==", "owner"); err != nil {
				t.Fatal(err)
			}
			if err := c.st.InsertReportSigningKey(ctx, project, "k1", "cHViMg==", "owner"); !errors.Is(err, ErrReportSigningKeyExists) {
				t.Fatalf("a key_id was re-registered: %v", err)
			}
			if k, err := c.st.GetReportSigningKey(ctx, project, "k1"); err != nil || k.PublicKey != "cHViMQ==" || k.RevokedAt != "" {
				t.Fatalf("key = %+v, %v", k, err)
			}
			if _, err := c.st.GetReportSigningKey(ctx, project, "nope"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing key: %v", err)
			}
			if err := c.st.RevokeReportSigningKey(ctx, project, "k1"); err != nil {
				t.Fatal(err)
			}
			if err := c.st.RevokeReportSigningKey(ctx, project, "k1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("revoked twice: %v", err)
			}
			keys, err := c.st.ListReportSigningKeys(ctx, project)
			if err != nil || len(keys) != 1 || keys[0].RevokedAt == "" {
				t.Fatalf("keys = %+v, %v; a revoked key stays listed", keys, err)
			}
			if err := c.st.InsertReportSigningKey(ctx, project, "k1", "cHViMg==", "owner"); !errors.Is(err, ErrReportSigningKeyExists) {
				t.Fatalf("a revoked key_id was reused: %v", err)
			}

			if _, err := c.st.GetReportChainStatus(ctx, exec); !errors.Is(err, ErrNotFound) {
				t.Fatalf("status before completion: %v", err)
			}
			if err := c.st.SetReportChainStatus(ctx, ReportChainStatusRow{ExecutionID: exec, ProjectID: project,
				Status: ReportChainBroken, Detail: " detail "}); err != nil {
				t.Fatal(err)
			}
			if err := c.st.SetReportChainStatus(ctx, ReportChainStatusRow{ExecutionID: exec, ProjectID: project,
				Status: ReportChainSigned, EndHash: "c", EndKeyID: "k1", EndSignature: "sig"}); err != nil {
				t.Fatal(err)
			}
			got2, err := c.st.GetReportChainStatus(ctx, exec)
			if err != nil || got2.Status != ReportChainSigned || got2.Detail != "" || got2.EndHash != "c" ||
				got2.EndKeyID != "k1" || got2.EndSignature != "sig" || got2.ProjectID != project {
				t.Fatalf("status = %+v %v", got2, err)
			}
		})
	}
}

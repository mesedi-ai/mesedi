package main

// The public dev project, opt-in since 2026-11-16. Moved out of main.go so
// the startup file stays under the size limit (foundation-audit B8).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"mesedi/backend/internal/store"
)

// bootstrapDevProject creates the "dev" project, the public test API key
// and a verified owner email, for local development only. It runs only
// through reconcileDevProject, with MESEDI_DEV_BOOTSTRAP=1. Idempotent:
// each piece is created only when missing. The key value is intentionally
// non-secret and hardcoded; never enable this anywhere reachable by others.
// Takes store.Store so it works against either SQLite or Postgres.
func bootstrapDevProject(ctx context.Context, st store.Store, logger *slog.Logger) error {
	existing, err := st.GetProject(ctx, devProjectID)
	switch {
	case err == nil && strings.TrimSpace(existing.OwnerEmail) == "":
		// Created before 2026-11-16 with no owner email. An empty email
		// can never be verified, so every request with the dev key is
		// refused with email_not_verified. There is no store method to
		// set an owner email after the fact, and this is a local dev
		// database, so say exactly what to do instead of guessing.
		logger.Warn("dev project predates MESEDI_DEV_BOOTSTRAP and has no owner email; "+
			"its key will be refused with email_not_verified. Delete the local database file to recreate it.",
			"project_id", devProjectID)
		return nil
	case err == nil:
		logger.Debug("dev project already exists", "project_id", devProjectID)
	case errors.Is(err, store.ErrNotFound):
		if err := st.CreateProject(ctx, &store.Project{
			ProjectID:  devProjectID,
			Name:       "Local Dev",
			OwnerEmail: devOwnerEmail,
		}); err != nil {
			return fmt.Errorf("create dev project: %w", err)
		}
	default:
		return fmt.Errorf("read dev project: %w", err)
	}

	if _, err := st.GetAPIKeyByHash(ctx, devKeyHash); errors.Is(err, store.ErrNotFound) {
		if err := st.CreateAPIKey(ctx, &store.APIKey{
			KeyID:     devKeyID,
			ProjectID: devProjectID,
			KeyHash:   devKeyHash,
			KeyPrefix: "mesedi_sk_dev",
			Name:      "Local dev key (public; MESEDI_DEV_BOOTSTRAP=1 only)",
		}); err != nil {
			return fmt.Errorf("create dev api key: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("read dev api key: %w", err)
	}

	// The README quickstart sends requests with the dev key, and every
	// customer route requires a verified owner email. The address is
	// under .invalid, which can never receive mail.
	if err := st.MarkEmailVerified(ctx, devOwnerEmail, "dev_bootstrap"); err != nil {
		return fmt.Errorf("verify dev owner email: %w", err)
	}
	logger.Info("dev project ready", "project_id", devProjectID, "key_id", devKeyID)
	return nil
}

// The public dev project. The key's plaintext, "mesedi_sk_dev_local_only",
// is printed in the README, so anyone can use it against any server that
// holds it.
const (
	devBootstrapEnv = "MESEDI_DEV_BOOTSTRAP"
	devProjectID    = "proj-dev"
	devKeyID        = "key-dev"
	devOwnerEmail   = "dev@local.invalid"
	// SHA-256 of the literal string "mesedi_sk_dev_local_only". Verify
	// via: echo -n "mesedi_sk_dev_local_only" | sha256sum
	devKeyHash = "63aee0bafbf5a68577021746b028842f70d922c2809776e1a1de0ecf6fc7fb33"
)

// reconcileDevProject decides, at every start, whether the public dev key
// may exist on this server.
//
// Until 2026-11-16 the dev project was created on every server not running
// on Fly, which includes every self-hosted one. One accident kept that
// harmless: the project had no owner email, so the email-verification gate
// refused the key everywhere. A self-hoster who set
// MESEDI_DISABLE_EMAIL_VERIFY_GATE=1 to skip email setup opened their server
// to a key printed in our README. The same accident broke the README's own
// quickstart with email_not_verified.
//
// Now (Robert's decision, 2026-11-16): the dev project is created only when
// MESEDI_DEV_BOOTSTRAP=1 is set and the server is not on Fly. On every other
// start the public key is removed if present, so servers that already hold
// it are cleaned on their next restart.
func reconcileDevProject(ctx context.Context, st store.Store, logger *slog.Logger, optedIn, onFly bool) error {
	if optedIn && !onFly {
		return bootstrapDevProject(ctx, st, logger)
	}
	if optedIn && onFly {
		logger.Warn("MESEDI_DEV_BOOTSTRAP ignored on Fly (FLY_APP_NAME set)")
	}
	return removePublicDevKey(ctx, st, logger)
}

// removePublicDevKey deletes the API key whose hash matches the public dev
// key, whatever key_id it was stored under. A no-op when absent.
func removePublicDevKey(ctx context.Context, st store.Store, logger *slog.Logger) error {
	key, err := st.GetAPIKeyByHash(ctx, devKeyHash)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up public dev key: %w", err)
	}
	if err := st.DeleteAPIKeyByID(ctx, key.KeyID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("remove public dev key: %w", err)
	}
	logger.Warn("removed the public dev key; set "+devBootstrapEnv+"=1 for local development",
		"key_id", key.KeyID, "project_id", key.ProjectID)
	return nil
}

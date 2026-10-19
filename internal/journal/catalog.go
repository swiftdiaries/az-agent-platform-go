package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// CatalogSnapshot is one captured tools/list result and the scope it was captured
// under. Tenant and Principal are the owner's; CommitSnapshot fills them in.
type CatalogSnapshot struct {
	Digest                                    string
	Tenant, Principal                         string
	ProductVersion, EndpointID, ServerVersion string
	Descriptors                               json.RawMessage // JSON array of raw tools/list descriptors
	CapturedAt                                time.Time
}

// CatalogPin is a conversation's pinned snapshot. Age is the database clock minus
// ValidatedAt at read time.
type CatalogPin struct {
	CatalogSnapshot
	ValidatedAt time.Time
	Age         time.Duration
}

// CatalogDigest is the sha256 of the canonical JSON of the snapshot's identity:
// tenant, principal, product version, endpoint ID, server version and descriptors.
// encoding/json compacts the descriptors, so insignificant whitespace is irrelevant.
func CatalogDigest(s CatalogSnapshot) (string, error) {
	data, err := json.Marshal(struct {
		Tenant         string          `json:"tenant"`
		Principal      string          `json:"principal"`
		ProductVersion string          `json:"product_version"`
		EndpointID     string          `json:"endpoint_id"`
		ServerVersion  string          `json:"server_version"`
		Descriptors    json.RawMessage `json:"descriptors"`
	}{s.Tenant, s.Principal, s.ProductVersion, s.EndpointID, s.ServerVersion, s.Descriptors})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func isJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '[' && json.Valid(trimmed)
}

// CommitSnapshot stores the snapshot (idempotently), makes it the latest for the
// owner's tenant and principal, pins it to the owner's conversation and run, and
// refreshes the conversation's validation time. When a different snapshot was
// pinned before, it appends a catalog_changed event. It returns the digest.
func (s *Store) CommitSnapshot(ctx context.Context, o Owner, snap CatalogSnapshot) (string, error) {
	c := o.Command
	if snap.ProductVersion == "" || snap.EndpointID == "" || !isJSONArray(snap.Descriptors) {
		return "", ErrState
	}
	if snap.Tenant != "" && snap.Tenant != c.Tenant || snap.Principal != "" && snap.Principal != c.Principal {
		return "", ErrState
	}
	snap.Tenant, snap.Principal = c.Tenant, c.Principal
	digest, err := CatalogDigest(snap)
	if err != nil {
		return "", ErrState
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		var previous, previousVersion *string
		if err := tx.QueryRow(ctx, "SELECT catalog_digest,product_version FROM agent_conversations WHERE id=$1", c.ThreadID).Scan(&previous, &previousVersion); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO agent_catalog_snapshots(digest,tenant,principal,product_version,endpoint_id,server_version,descriptors) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (digest) DO NOTHING", digest, snap.Tenant, snap.Principal, snap.ProductVersion, snap.EndpointID, snap.ServerVersion, []byte(snap.Descriptors)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO agent_catalog_latest(tenant,principal,digest) VALUES($1,$2,$3) ON CONFLICT (tenant,principal) DO UPDATE SET digest=EXCLUDED.digest,updated_at=clock_timestamp()", snap.Tenant, snap.Principal, digest); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE agent_conversations SET catalog_digest=$2,product_version=$3,catalog_validated_at=clock_timestamp() WHERE id=$1", c.ThreadID, digest, snap.ProductVersion); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE agent_runs SET catalog_digest=$3 WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID, digest); err != nil {
			return err
		}
		if previous == nil || *previous == digest {
			return nil
		}
		oldVersion := ""
		if previousVersion != nil {
			oldVersion = *previousVersion
		}
		detail, err := json.Marshal(map[string]string{"old_digest": *previous, "new_digest": digest, "old_product_version": oldVersion, "new_product_version": snap.ProductVersion})
		if err != nil {
			return err
		}
		return appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: "catalog_changed", Detail: detail})
	})
	if err != nil {
		return "", err
	}
	return digest, nil
}

// RecordRunCatalog records, under the ownership fence, that the owner's run used
// the conversation's pinned snapshot. The digest must be that pin. Reusing the
// pin is a revalidation, so it also refreshes the conversation's validation time.
func (s *Store) RecordRunCatalog(ctx context.Context, o Owner, digest string) error {
	c := o.Command
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		var pinned bool
		if err := tx.QueryRow(ctx, "SELECT COALESCE(catalog_digest=$2,false) FROM agent_conversations WHERE id=$1", c.ThreadID, digest).Scan(&pinned); err != nil {
			return err
		}
		if !pinned {
			return ErrState
		}
		if _, err := tx.Exec(ctx, "UPDATE agent_conversations SET catalog_validated_at=clock_timestamp() WHERE id=$1", c.ThreadID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE agent_runs SET catalog_digest=$3 WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID, digest)
		return err
	})
}

// ConversationPin reads the owner's conversation pin under the ownership fence;
// ok is false when the conversation has no pinned snapshot.
func (s *Store) ConversationPin(ctx context.Context, o Owner) (CatalogPin, bool, error) {
	var pin CatalogPin
	var ok bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		var descriptors []byte
		var age int64
		err := tx.QueryRow(ctx, "SELECT s.digest,s.tenant,s.principal,s.product_version,s.endpoint_id,s.server_version,s.descriptors,s.captured_at,c.catalog_validated_at,(extract(epoch FROM clock_timestamp()-c.catalog_validated_at)*1000000)::bigint FROM agent_conversations c JOIN agent_catalog_snapshots s ON s.digest=c.catalog_digest WHERE c.id=$1", o.Command.ThreadID).Scan(&pin.Digest, &pin.Tenant, &pin.Principal, &pin.ProductVersion, &pin.EndpointID, &pin.ServerVersion, &descriptors, &pin.CapturedAt, &pin.ValidatedAt, &age)
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		pin.Descriptors = descriptors
		pin.Age = time.Duration(age) * time.Microsecond
		ok = true
		return nil
	})
	if err != nil {
		return CatalogPin{}, false, err
	}
	return pin, ok, nil
}

// LatestSnapshot returns the most recently committed snapshot for a tenant and principal.
func (s *Store) LatestSnapshot(ctx context.Context, tenant, principal string) (CatalogSnapshot, bool, error) {
	var snap CatalogSnapshot
	var descriptors []byte
	err := s.pool.QueryRow(ctx, "SELECT s.digest,s.tenant,s.principal,s.product_version,s.endpoint_id,s.server_version,s.descriptors,s.captured_at FROM agent_catalog_latest l JOIN agent_catalog_snapshots s ON s.digest=l.digest WHERE l.tenant=$1 AND l.principal=$2", tenant, principal).Scan(&snap.Digest, &snap.Tenant, &snap.Principal, &snap.ProductVersion, &snap.EndpointID, &snap.ServerVersion, &descriptors, &snap.CapturedAt)
	if err == pgx.ErrNoRows {
		return CatalogSnapshot{}, false, nil
	}
	if err != nil {
		return CatalogSnapshot{}, false, err
	}
	snap.Descriptors = descriptors
	return snap, true, nil
}

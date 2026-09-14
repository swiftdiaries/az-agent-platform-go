package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
)

// IdentityMap is Chat's authority. Agent tables receive only the opaque values.
// ON CONFLICT returns the existing mapping without replacing its random identity.
type IdentityMap struct{ pool *pgxpool.Pool }

func opaqueID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (m IdentityMap) resolve(ctx context.Context, principal, externalThread, externalRun string, create bool) (thread, run, communication string, err error) {
	if !create {
		err = m.pool.QueryRow(ctx, `SELECT t.id,r.id,r.communication_id FROM chat_threads t JOIN chat_runs r ON r.thread_id=t.id
   WHERE t.principal=$1 AND t.external_id=$2 AND r.external_id=$3`, principal, externalThread, externalRun).Scan(&thread, &run, &communication)
		if err == pgx.ErrNoRows {
			err = platform.ErrForbidden
		}
		return
	}
	err = pgx.BeginFunc(ctx, m.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO chat_threads(principal,external_id,id) VALUES($1,$2,$3)
   ON CONFLICT(principal,external_id) DO UPDATE SET external_id=EXCLUDED.external_id RETURNING id`, principal, externalThread, opaqueID()).Scan(&thread)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO chat_runs(thread_id,external_id,id,communication_id) VALUES($1,$2,$3,$4)
   ON CONFLICT(thread_id,external_id) DO UPDATE SET external_id=EXCLUDED.external_id RETURNING id,communication_id`, thread, externalRun, opaqueID(), opaqueID()).Scan(&run, &communication)
	})
	return
}

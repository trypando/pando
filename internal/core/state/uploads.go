package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// Uploads keeps uploaded source archives in the database (R-417, R-262), so
// every control host reads the one uploaded on any other with no directory
// shared between them. Migration 000086 says why the archive is stored in
// chunks of bytea.
type Uploads struct {
	db *DB
}

// NewUploads returns the upload store.
func NewUploads(db *DB) *Uploads { return &Uploads{db: db} }

// uploadChunkBytes is one stored chunk: small enough that a read or a write
// holds little of a 256 MiB archive in memory at once, large enough that one
// is a few dozen rows.
const uploadChunkBytes = 4 << 20

const couldNotStoreUpload = "Pando could not store the upload."

// PutUpload stores r as appID's uploaded archive, replacing any before it.
//
// One transaction: a deploy reading the app's upload while this runs reads the
// previous archive whole, never part of each. The app_uploads row is written
// first, which locks it, so two uploads to one app take turns. The
// transaction holds a connection until r ends, so r is a file already on disk
// rather than a request body a client may send slowly (R-417). An app that
// is deleted is errs.NotFound (lockLiveApp).
func (u *Uploads) PutUpload(ctx context.Context, appID string, r io.Reader) error {
	_, err := u.put(ctx, appID, r, `
		INSERT INTO app_uploads (app_id, sha256, size) VALUES ($1, repeat('0', 64), 0)
		ON CONFLICT (app_id) DO UPDATE SET updated_at = now()`)
	return err
}

// AddUpload stores r as appID's uploaded archive only if the app has none,
// and reports whether it did. It is how an archive file from before R-417 is
// moved in: an upload stored meanwhile is newer, and is kept rather than
// overwritten by the file. An app that is deleted, or was never created, is
// errs.NotFound (lockLiveApp).
func (u *Uploads) AddUpload(ctx context.Context, appID string, r io.Reader) (bool, error) {
	return u.put(ctx, appID, r, `
		INSERT INTO app_uploads (app_id, sha256, size) VALUES ($1, repeat('0', 64), 0)
		ON CONFLICT (app_id) DO NOTHING`)
}

// put writes the app_uploads row with claim, which holds the row's lock for
// the rest of the transaction, then replaces the chunks with r's. It reports
// false, writing nothing, when claim wrote no row.
//
// The row's digest is a placeholder until the update below, before the
// commit, so no reader sees it.
func (u *Uploads) put(ctx context.Context, appID string, r io.Reader, claim string) (bool, error) {
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return false, errs.Wrap(errs.Internal, couldNotStoreUpload, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockLiveApp(ctx, tx, appID); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, claim, appID)
	if isForeignKeyViolation(err) {
		return false, errAppGone(appID)
	}
	if err != nil {
		return false, errs.Wrap(errs.Internal, couldNotStoreUpload, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app_upload_chunks WHERE app_id = $1`, appID); err != nil {
		return false, errs.Wrap(errs.Internal, couldNotStoreUpload, err)
	}

	sum := sha256.New()
	size, err := writeChunks(ctx, tx, appID, io.TeeReader(r, sum))
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE app_uploads SET sha256 = $2, size = $3, updated_at = now() WHERE app_id = $1`,
		appID, hex.EncodeToString(sum.Sum(nil)), size); err != nil {
		return false, errs.Wrap(errs.Internal, couldNotStoreUpload, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, errs.Wrap(errs.Internal, couldNotStoreUpload, err)
	}
	return true, nil
}

func errAppGone(appID string) error {
	return errs.Newf(errs.NotFound, "The app %s no longer exists, so its upload was not stored.", appID)
}

// lockLiveApp refuses an app that is deleted or was never created, and holds
// its row until the transaction ends, so the app cannot be deleted while its
// upload is stored (R-417, R-224). A deleted app's row is kept, so the
// foreign key alone stores an upload for it that nothing removes, and every
// backup carries; deleting the app removes its upload (migration 000095).
func lockLiveApp(ctx context.Context, tx pgx.Tx, appID string) error {
	var live bool
	err := tx.QueryRow(ctx, `SELECT deleted_at IS NULL FROM apps WHERE id = $1 FOR SHARE`, appID).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
		return errAppGone(appID)
	}
	if err != nil {
		return errs.Wrap(errs.Internal, couldNotStoreUpload, err)
	}
	return nil
}

// writeChunks copies r into the app's chunks and returns how many bytes it
// wrote. A read error is returned as it came, so the caller can still tell a
// request body over its limit from a failure here.
func writeChunks(ctx context.Context, tx pgx.Tx, appID string, r io.Reader) (int64, error) {
	buf := make([]byte, uploadChunkBytes)
	var size int64
	for seq := 0; ; seq++ {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if _, execErr := tx.Exec(ctx,
				`INSERT INTO app_upload_chunks (app_id, seq, data) VALUES ($1, $2, $3)`,
				appID, seq, buf[:n]); execErr != nil {
				return size, errs.Wrap(errs.Internal, couldNotStoreUpload, execErr)
			}
			size += int64(n)
		}
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			return size, nil
		case err != nil:
			return size, err
		}
	}
}

// OpenUpload returns appID's uploaded archive, read chunk by chunk. ok is false
// when the app has none. The reader fails at its end if what was read is not
// what was stored. Close it: it holds a database connection until then.
func (u *Uploads) OpenUpload(ctx context.Context, appID string) (io.ReadCloser, bool, error) {
	// Repeatable read, so the digest and the chunks are one upload's even if
	// another replaces it mid-read.
	tx, err := u.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, false, errs.Wrap(errs.Internal, couldNotReadUpload, err)
	}
	var digest string
	var size int64
	err = tx.QueryRow(ctx, `SELECT sha256, size FROM app_uploads WHERE app_id = $1`, appID).Scan(&digest, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		return nil, false, nil
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, false, errs.Wrap(errs.Internal, couldNotReadUpload, err)
	}
	rows, err := tx.Query(ctx, `SELECT data FROM app_upload_chunks WHERE app_id = $1 ORDER BY seq`, appID)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, false, errs.Wrap(errs.Internal, couldNotReadUpload, err)
	}
	return &uploadReader{ctx: ctx, tx: tx, rows: rows, want: digest, size: size, sum: sha256.New()}, true, nil
}

const couldNotReadUpload = "Pando could not read the uploaded source."

// DeleteUpload removes appID's uploaded archive. An app with none is not an
// error.
func (u *Uploads) DeleteUpload(ctx context.Context, appID string) error {
	if _, err := u.db.Exec(ctx, `DELETE FROM app_uploads WHERE app_id = $1`, appID); err != nil {
		return errs.Wrap(errs.Internal, "Pando could not remove the app's uploaded source.", err)
	}
	return nil
}

// HasUpload reports whether appID has an uploaded archive in the database.
func (u *Uploads) HasUpload(ctx context.Context, appID string) (bool, error) {
	var ok bool
	if err := u.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_uploads WHERE app_id = $1)`, appID).Scan(&ok); err != nil {
		return false, errs.Wrap(errs.Internal, couldNotReadUpload, err)
	}
	return ok, nil
}

// uploadReader streams an upload's chunks inside the transaction that read its
// digest.
type uploadReader struct {
	ctx  context.Context
	tx   pgx.Tx
	rows pgx.Rows
	cur  []byte
	want string
	size int64
	read int64
	sum  hash.Hash
	// end is what every read answers once the chunks are done: io.EOF, or
	// why the archive cannot be trusted, said again to every later read.
	end error
}

func (r *uploadReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		if r.end != nil {
			return 0, r.end
		}
		if !r.rows.Next() {
			r.end = r.finish()
			return 0, r.end
		}
		if err := r.rows.Scan(&r.cur); err != nil {
			r.end = errs.Wrap(errs.Internal, couldNotReadUpload, err)
			return 0, r.end
		}
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	r.read += int64(n)
	r.sum.Write(p[:n])
	return n, nil
}

// finish checks, at the last chunk, that what was read is what was stored.
func (r *uploadReader) finish() error {
	if err := r.rows.Err(); err != nil {
		return errs.Wrap(errs.Internal, couldNotReadUpload, err)
	}
	if r.read != r.size || hex.EncodeToString(r.sum.Sum(nil)) != r.want {
		return errs.New(errs.Internal, "This app's uploaded source in the database does not match its recorded digest.").
			WithRemedy("Run `pando deploy .` from the app's directory again.")
	}
	return io.EOF
}

func (r *uploadReader) Close() error {
	r.rows.Close()
	return r.tx.Rollback(r.ctx)
}

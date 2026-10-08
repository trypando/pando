package state

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// Services records which provisioned service fills which slot (R-131).
type Services struct{ db *DB }

func NewServices(db *DB) *Services { return &Services{db: db} }

// ServiceInstance is one provisioned service.
type ServiceInstance struct {
	ID            string        `json:"id"`
	AppID         string        `json:"app_id"`
	SlotKey       string        `json:"slot_key"`
	SlotType      spec.SlotType `json:"slot_type"`
	AdapterRef    string        `json:"adapter_ref"`
	Handle        string        `json:"handle"`
	ConnectionRef string        `json:"secret_key"`
}

// NewID returns an ID for a service about to be provisioned.
//
// Minted before the adapter is called and passed into Provision, so a deploy
// that dies between the adapter answering and the row being written can retry
// with the same ID and get the same names back. Without that, the retry stands
// up a second database and the first becomes a volume nobody can name.
func (s *Services) NewID() string { return id.New(id.Service) }

// ForApp lists an app's provisioned services.
func (s *Services) ForApp(ctx context.Context, appID string) ([]ServiceInstance, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, app_id, slot_key, slot_type, adapter_ref, handle, secret_key
		FROM service_instances WHERE app_id = $1 ORDER BY slot_key`, appID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read this app's provisioned services.", err)
	}
	defer rows.Close()

	out := make([]ServiceInstance, 0)
	for rows.Next() {
		var v ServiceInstance
		if err := rows.Scan(&v.ID, &v.AppID, &v.SlotKey, &v.SlotType,
			&v.AdapterRef, &v.Handle, &v.ConnectionRef); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read this app's provisioned services.", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Get returns the instance filling one slot, if there is one.
func (s *Services) Get(ctx context.Context, appID, slotKey string) (ServiceInstance, bool, error) {
	var v ServiceInstance
	err := s.db.QueryRow(ctx, `
		SELECT id, app_id, slot_key, slot_type, adapter_ref, handle, secret_key
		FROM service_instances WHERE app_id = $1 AND slot_key = $2`, appID, slotKey).
		Scan(&v.ID, &v.AppID, &v.SlotKey, &v.SlotType, &v.AdapterRef, &v.Handle, &v.ConnectionRef)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ServiceInstance{}, false, nil
	case err != nil:
		return ServiceInstance{}, false, errs.Wrap(errs.Internal, "Could not read the provisioned service.", err)
	}
	return v, true, nil
}

// Record stores a newly provisioned service.
//
// ON CONFLICT DO NOTHING rather than DO UPDATE: if a row already fills this
// slot, that row names the database holding the app's data and the caller's
// newer instance is an empty one from a retry. Overwriting would point the app
// at the empty database and leave the real one unreferenced.
func (s *Services) Record(ctx context.Context, v ServiceInstance) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO service_instances (id, app_id, slot_key, slot_type, adapter_ref, handle, secret_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (app_id, slot_key) DO NOTHING`,
		v.ID, v.AppID, v.SlotKey, string(v.SlotType), v.AdapterRef, v.Handle, v.ConnectionRef)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the provisioned service.", err)
	}
	return nil
}

// Release forgets a provisioned service.
func (s *Services) Release(ctx context.Context, id string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM service_instances WHERE id = $1`, id); err != nil {
		return errs.Wrap(errs.Internal, "Could not release the provisioned service.", err)
	}
	return nil
}

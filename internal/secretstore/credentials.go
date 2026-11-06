package secretstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrGenerationConflict reports that a Credential changed (login, refresh,
// forget) between a snapshot and a commit.
var ErrGenerationConflict = errors.New("credential changed under us")

// Credential returns a login-managed record and its generation. ok=false
// means no record (including no store at all).
func (s *Store) Credential(ctx context.Context, key string) (record json.RawMessage, generation uint64, ok bool, err error) {
	p, err := s.View(ctx)
	if errors.Is(err, ErrNoStore) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	c, found := p.Credentials[key]
	if !found {
		return nil, 0, false, nil
	}

	return c.Record, c.Generation, true, nil
}

// Generation returns the record's generation or, when it is absent, the
// tombstone generation of a forgotten record (0 when neither exists).
func (s *Store) Generation(ctx context.Context, key string) (uint64, error) {
	p, err := s.View(ctx)
	if errors.Is(err, ErrNoStore) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	return generationOf(p, key), nil
}

func generationOf(p *Payload, key string) uint64 {
	if c, ok := p.Credentials[key]; ok {
		return c.Generation
	}

	return p.Tombstones[key]
}

// PutCredential stores a record and returns its new generation. build
// receives that generation so it can embed it in the record. When expect is
// non-nil the write succeeds only while the current generation still equals
// *expect (ErrGenerationConflict otherwise): the compare-before-save of
// interactive work and refresh rotation. It creates the store on first use.
func (s *Store) PutCredential(ctx context.Context, key string, expect *uint64, build func(generation uint64) (json.RawMessage, error)) (uint64, error) {
	var newGen uint64
	err := s.Update(ctx, func(p *Payload) error {
		cur := generationOf(p, key)
		if expect != nil && *expect != cur {
			return fmt.Errorf("%w: %s (snapshot %d, current %d)", ErrGenerationConflict, key, *expect, cur)
		}
		newGen = cur + 1
		rec, err := build(newGen)
		if err != nil {
			return err
		}
		p.Credentials[key] = Credential{Generation: newGen, Record: rec}
		delete(p.Tombstones, key)

		return nil
	})
	if err != nil {
		return 0, err
	}

	return newGen, nil
}

// RemoveCredential deletes a record and leaves a tombstone one generation
// above it, so a commit snapshotted before the delete conflicts instead of
// resurrecting it. Removing an absent record is not an error and creates
// nothing.
func (s *Store) RemoveCredential(ctx context.Context, key string) error {
	if !s.Exists() {
		return nil
	}

	return s.Update(ctx, func(p *Payload) error {
		c, ok := p.Credentials[key]
		if !ok {
			return nil
		}
		delete(p.Credentials, key)
		p.Tombstones[key] = c.Generation + 1

		return nil
	})
}

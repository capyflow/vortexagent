package sessionstore

import "context"

type Store interface {
	Save(ctx context.Context, s *Session) error
	Load(ctx context.Context, id string) (*Session, error)
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]*Session, error)
}

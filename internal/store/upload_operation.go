package store

import "context"

type uploadOperationKey struct{}
type uploadOperation struct {
	ID      int64
	ItemKey string
}
type intakeParentKey struct{}

func WithIntakeParent(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, intakeParentKey{}, id)
}

// The intake transaction owns the operation association. A cancellation that
// arrives while bytes are being uploaded therefore cannot admit a later file.
func WithUploadOperation(ctx context.Context, id int64, itemKey string) context.Context {
	return context.WithValue(ctx, uploadOperationKey{}, uploadOperation{id, itemKey})
}

package source

import "context"

type appKey struct{}

// ForApp records which app a fetch is for, so a source connection's use is
// audited against it.
func ForApp(ctx context.Context, appID string) context.Context {
	return context.WithValue(ctx, appKey{}, appID)
}

// AppFrom is the app ForApp recorded, or empty.
func AppFrom(ctx context.Context) string {
	id, _ := ctx.Value(appKey{}).(string)
	return id
}

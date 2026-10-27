package s3

// SetPartSize makes parts small enough for a test to send several.
func SetPartSize(a *Adapter, n int) { a.partSize = n }

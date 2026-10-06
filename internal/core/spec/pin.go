package spec

// DropStalePin clears an image app's digest when next names a different
// image than prev and still carries prev's digest (issue #41).
//
// A digest pins what an image reference resolved to, so it is only true of
// the reference it was resolved from. Someone who edits ghcr.io/acme/web:1 to
// :2 and leaves the digest would otherwise go on running :1 — the pull is by
// digest — with a spec that says :2. Cleared rather than refused: the next
// deploy pins :2 the way it pins any unpinned image, in a revision of its own.
// A digest that was changed alongside the image is left alone; that is a
// pin somebody chose.
func DropStalePin(prev, next *AppSpec) {
	if prev == nil || next == nil || next.Source.Type != SourceImage || next.Source.Digest == "" {
		return
	}
	if next.Source.Image != prev.Source.Image && next.Source.Digest == prev.Source.Digest {
		next.Source.Digest = ""
	}
}

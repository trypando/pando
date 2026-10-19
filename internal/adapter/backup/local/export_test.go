package local

// SetMountinfoPath points the mount check at a fake mount table for one test.
func SetMountinfoPath(p string) (restore func()) {
	old := mountinfoPath
	mountinfoPath = p
	return func() { mountinfoPath = old }
}

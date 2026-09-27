package ingest

import (
	"os"

	"dboss/internal/logstore"
	"dboss/internal/logx"
)

// releaseMin is how much ingested head an app log file may keep before it is released, so the
// file stays about this size instead of growing for the life of the app.
const releaseMin = 1 << 20

// release gives the ingested head of an app log file back to the filesystem once it passes
// releaseMin. The file is never renamed or recreated: an app keeps writing through its open
// handle, so a renamed file would take its next lines along with it. collapseHead cuts the head in
// place instead, serialized with the app's appends by the kernel, so no line can slip between the
// cut and a write. The app has to open the file for appending, which every logger does.
//
// The shifted offset is saved before the cut: a crash between the two re-reads the released bytes
// (duplicate rows), it never skips unread ones. A cut that fails puts the old offset back, and a
// file whose filesystem cannot cut is left alone from then on.
func (m *Module) release(app string, at logstore.TailOffset) error {
	if !canRelease || at.Offset < releaseMin || m.unreleasable[at.Path] {
		return nil
	}
	file, err := os.OpenFile(at.Path, os.O_WRONLY, 0)
	if err != nil {
		m.stopReleasing(at.Path, err)
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if inodeOf(info) != at.Inode {
		// Replaced since the read; the next pass starts the new file over.
		return nil
	}
	block := blockSize(info)
	cut := at.Offset / block * block
	// A collapse must end before the end of the file.
	if cut >= info.Size() {
		cut -= block
	}
	if cut <= 0 {
		return nil
	}
	released := at
	released.Offset -= cut
	released.Base += cut
	if err := m.store.SaveTailOffset(app, released); err != nil {
		return err
	}
	if err := collapseHead(file, cut); err != nil {
		if restore := m.store.SaveTailOffset(app, at); restore != nil {
			return restore
		}
		m.stopReleasing(at.Path, err)
	}
	return nil
}

func (m *Module) stopReleasing(path string, err error) {
	m.unreleasable[path] = true
	logx.Warnf("app log %s is not trimmed after ingest: %v", path, err)
}

package ingest

import (
	"os"
	"syscall"
)

const canRelease = true

// fallocCollapseRange is FALLOC_FL_COLLAPSE_RANGE from linux/falloc.h. ext4 and xfs support it;
// others answer EOPNOTSUPP and the file is left as it is.
const fallocCollapseRange = 0x08

// collapseHead removes the first n bytes of the file and shifts the rest down, under the inode
// lock the app's appends take too. n must be a multiple of the filesystem block size and end
// before the end of the file.
func collapseHead(file *os.File, n int64) error {
	return syscall.Fallocate(int(file.Fd()), fallocCollapseRange, 0, n)
}

// blockSize is the unit a collapse must be aligned to. st_blksize is the filesystem block size or
// a multiple of it, so a cut aligned to it is aligned to the block.
func blockSize(info os.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Blksize > 0 {
		return int64(stat.Blksize)
	}
	return 4096
}

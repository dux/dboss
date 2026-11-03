package cli

import (
	"context"
	"os"
	"sync"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// terminalKeys switches a terminal stdin to single-key input and streams every byte typed. Only
// line buffering and echo go: Ctrl-C still signals and output is untouched. Without a terminal on
// stdin the channel is nil. restore puts the terminal back and may be called more than once.
func (c CLI) terminalKeys() (<-chan byte, func()) {
	in, ok := c.In.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return nil, func() {}
	}
	fd := int(in.Fd())
	saved, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, func() {}
	}
	single := *saved
	single.Lflag &^= unix.ICANON | unix.ECHO
	single.Cc[unix.VMIN], single.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &single); err != nil {
		return nil, func() {}
	}
	var once sync.Once
	restore := func() { once.Do(func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, saved) }) }
	keys := make(chan byte)
	go func() {
		defer close(keys)
		buffer := make([]byte, 64)
		for {
			n, err := in.Read(buffer)
			for _, key := range buffer[:n] {
				keys <- key
			}
			if err != nil {
				return
			}
		}
	}()
	return keys, restore
}

// restartOnKeys restarts the dev session's app on every r, without asking, until the session
// stops. A restart runs beside the loop, so r pressed during a roll queues one more.
func restartOnKeys(ctx context.Context, keys <-chan byte, app string, say func(string), restart func() error) {
	for {
		select {
		case key, open := <-keys:
			if !open {
				keys = nil
				continue
			}
			if key != 'r' && key != 'R' {
				continue
			}
			say("restarting " + app)
			go func() {
				if err := restart(); err != nil {
					say("restart " + app + " failed: " + err.Error())
				}
			}()
		case <-ctx.Done():
			return
		}
	}
}

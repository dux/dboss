package apps

import (
	"fmt"
	"os"
	"path/filepath"
)

// Destroy removes one direct entry of root, an app folder.
func Destroy(root, name string) error {
	if name == "" || name == "." || filepath.Base(name) != name || !filepath.IsLocal(name) {
		return fmt.Errorf("invalid app name %q", name)
	}
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("app path %q is not a directory", path)
	}
	return os.RemoveAll(path)
}

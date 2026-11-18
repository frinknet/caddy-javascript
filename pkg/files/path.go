package files

import "path"

func Join(parts ...string) string {
	return path.Join(parts...)
}

//go:build !windows

package prepare

import "runtime"

// toolEnv : environnement réduit de l'outil (aucune variable de l'agent : ni clé, ni
// option cachée comme ZIPOPT), noms en UTF-8, fichiers temporaires dans tmp.
func toolEnv(tmp string) []string {
	locale := "C.UTF-8"
	if runtime.GOOS == "darwin" {
		locale = "en_US.UTF-8"
	}
	return []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=" + locale, "TMPDIR=" + tmp, "HOME=" + tmp}
}

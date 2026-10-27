//go:build windows

package guard

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// isLink : lien symbolique ou point de montage (jonction). Les autres points de
// reparse (fichiers à la demande de OneDrive, déduplication) sont des dossiers
// ordinaires pour la sauvegarde.
func isLink(p string) (bool, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return false, err
	}
	h, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(h)
	var info struct{ Attributes, ReparseTag uint32 }
	const fileAttributeTagInfo = 9
	if err := windows.GetFileInformationByHandleEx(h, fileAttributeTagInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return false, err
	}
	if info.Attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return false, nil
	}
	return info.ReparseTag == windows.IO_REPARSE_TAG_MOUNT_POINT || info.ReparseTag == windows.IO_REPARSE_TAG_SYMLINK, nil
}

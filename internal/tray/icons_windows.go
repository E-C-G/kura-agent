//go:build windows

package tray

import (
	"fmt"
	"unsafe"
)

// hicon crée l'icône Windows d'un état à la taille des petites icônes du système.
func hicon(icon Icon, frame int) (uintptr, error) {
	size, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	if size == 0 || size > 256 {
		size = 16
	}
	n := int(size)
	px := Pixels(icon, frame, n)

	bi := bitmapInfoHeader{Size: uint32(unsafe.Sizeof(bitmapInfoHeader{})), Width: int32(n), Height: -int32(n),
		Planes: 1, BitCount: 32}
	hdc, _, _ := procGetDC.Call(0)
	var bits unsafe.Pointer
	color, _, err := procCreateDIBSection.Call(hdc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	procReleaseDC.Call(0, hdc)
	if color == 0 || bits == nil {
		return 0, fmt.Errorf("CreateDIBSection : %v", err)
	}
	defer procDeleteObject.Call(color)
	copy(unsafe.Slice((*byte)(bits), len(px)), px)

	mask, _, err := procCreateBitmap.Call(uintptr(n), uintptr(n), 1, 1, 0)
	if mask == 0 {
		return 0, fmt.Errorf("CreateBitmap : %v", err)
	}
	defer procDeleteObject.Call(mask)

	ii := iconInfo{FIcon: 1, HbmMask: mask, HbmColor: color}
	h, _, err := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	if h == 0 {
		return 0, fmt.Errorf("CreateIconIndirect : %v", err)
	}
	return h, nil
}

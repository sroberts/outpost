// Package size writes byte counts the way people read them, so the portal and
// the CLI give the same directory the same size.
package size

import "strconv"

// Bytes is a size in decimal units (1 KB is 1000 bytes), as disk makers and
// file managers count, with a decimal place only where it says something.
func Bytes(n uint64) string {
	const units = "KMGTPE"
	if n < 1000 {
		return strconv.FormatUint(n, 10) + " B"
	}
	v := float64(n)
	i := -1
	for v >= 999.5 && i < len(units)-1 {
		v /= 1000
		i++
	}
	unit := string(units[i]) + "B"
	if v < 9.95 {
		return strconv.FormatFloat(v, 'f', 1, 64) + " " + unit
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " " + unit
}

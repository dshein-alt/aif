package sanitize

import "strconv"

func itoa(n int) string { return strconv.Itoa(n) }
func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	return s
}

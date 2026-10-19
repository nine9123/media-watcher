//go:build !linux

package scan

import "os"

func statMtime(st os.FileInfo) float64 {
	t := st.ModTime()
	return float64(t.Unix()) + float64(float64(t.Nanosecond())*1e-9)
}

// statCtime falls back to the mtime where there is no Stat_t.Ctim.
func statCtime(st os.FileInfo) float64 { return statMtime(st) }

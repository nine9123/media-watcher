package scan

import (
	"os"
	"syscall"
)

// statMtime is the mtime as Python's os.stat computes st_mtime (sec + nsec*1e-9),
// so values written by the Python version compare equal.
func statMtime(st os.FileInfo) float64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return float64(s.Mtim.Sec) + float64(float64(s.Mtim.Nsec)*1e-9)
	}
	return float64(st.ModTime().UnixNano()) / 1e9
}

func statCtime(st os.FileInfo) float64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return float64(s.Ctim.Sec) + float64(float64(s.Ctim.Nsec)*1e-9)
	}
	return statMtime(st)
}

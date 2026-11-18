package dashboard

// truncRunes cuts s to at most n characters, never in the middle of one: a byte slice
// through a multibyte character leaves invalid UTF-8, which Postgres refuses.
func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

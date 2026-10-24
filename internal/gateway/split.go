package gateway

import "strings"

// Split cuts text into chunks of at most max characters, so one logical reply
// becomes several platform messages. It breaks at a newline when one is
// reasonably close, then at a space, and hard-cuts otherwise; a chunk never
// splits a rune or starts with the whitespace that followed a break. An empty
// text produces no chunks, so callers never send an empty message.
//
// max counts runes. Telegram limits a message to 4096 UTF-16 code units, which
// is never fewer than 4096 runes, so a rune budget is safe there and simple
// everywhere else.
func Split(text string, limit int) []string {
	if limit <= 0 {
		limit = DefaultMaxChunk
	}
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return []string{text}
	}
	var out []string
	for len(runes) > 0 {
		if len(runes) <= limit {
			out = append(out, string(runes))
			break
		}
		cut := limit
		if i := lastIndex(runes[:limit], '\n'); i >= limit/2 {
			cut = i + 1 // keep the newline as the chunk's last character
		} else if i := lastIndex(runes[:limit], ' '); i >= limit/2 {
			cut = i
		}
		if cut < 1 {
			cut = 1
		}
		if chunk := strings.TrimRight(string(runes[:cut]), " \t\r\n"); chunk != "" {
			out = append(out, chunk)
		}
		runes = []rune(strings.TrimLeft(string(runes[cut:]), " \t\r\n"))
	}
	return out
}

func lastIndex(runes []rune, r rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

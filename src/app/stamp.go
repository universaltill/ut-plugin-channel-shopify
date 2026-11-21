package app

import "strconv"

// stamp is an RFC 3339 instant: whole seconds since the Unix epoch plus the
// fraction as nanoseconds. The shipped module may not import "time" (records
// are deterministic, .golangci.yml), and a plain string comparison is wrong
// across a fraction or an offset, so the one comparison the "Shopify edited it
// later" guard needs is done here.
type stamp struct{ sec, nano int64 }

func (a stamp) after(b stamp) bool {
	return a.sec > b.sec || (a.sec == b.sec && a.nano > b.nano)
}

// parseStamp reads YYYY-MM-DDTHH:MM:SS[.fraction](Z|+HH:MM|-HH:MM). Anything
// else is not ok: an unreadable time is never proof of anything.
func parseStamp(s string) (stamp, bool) {
	if len(s) < 20 || s[4] != '-' || s[7] != '-' || (s[10] != 'T' && s[10] != 't') || s[13] != ':' || s[16] != ':' {
		return stamp{}, false
	}
	num := func(from, to int) (int64, bool) {
		v, err := strconv.ParseInt(s[from:to], 10, 64)
		for _, c := range s[from:to] {
			if c < '0' || c > '9' {
				return 0, false
			}
		}
		return v, err == nil
	}
	y, ok1 := num(0, 4)
	mo, ok2 := num(5, 7)
	d, ok3 := num(8, 10)
	h, ok4 := num(11, 13)
	mi, ok5 := num(14, 16)
	sc, ok6 := num(17, 19)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || sc > 60 {
		return stamp{}, false
	}
	i := 19
	var nano int64
	if s[i] == '.' {
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i+1 {
			return stamp{}, false
		}
		frac := s[i+1 : j]
		if len(frac) > 9 {
			frac = frac[:9]
		}
		for len(frac) < 9 {
			frac += "0"
		}
		nano, _ = strconv.ParseInt(frac, 10, 64)
		i = j
	}
	var off int64
	switch zone := s[i:]; {
	case zone == "Z" || zone == "z":
	case len(zone) == 6 && (zone[0] == '+' || zone[0] == '-') && zone[3] == ':':
		oh, okh := num(i+1, i+3)
		om, okm := num(i+4, i+6)
		if !okh || !okm || oh > 23 || om > 59 {
			return stamp{}, false
		}
		off = oh*3600 + om*60
		if zone[0] == '-' {
			off = -off
		}
	default:
		return stamp{}, false
	}
	return stamp{sec: daysFromCivil(y, mo, d)*86400 + h*3600 + mi*60 + sc - off, nano: nano}, true
}

// daysFromCivil is the days since 1970-01-01 of a proleptic Gregorian date.
func daysFromCivil(y, m, d int64) int64 {
	if m <= 2 {
		y--
	}
	era := y / 400
	yoe := y - era*400
	mp := (m + 9) % 12
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

package period

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Fixed Go-distributed IANA data; never consult the host's zoneinfo or ZONEINFO.
//
//go:embed zoneinfo.zip
var zoneData []byte
var TZDataID = fmt.Sprintf("sha256:%x", sha256.Sum256(zoneData))
var zones sync.Map

func location(name string) (*time.Location, error) {
	if name == "UTC" {
		return time.UTC, nil
	}
	if name == "" || name == "Local" || strings.Contains(name, "..") {
		return nil, ErrPolicy
	}
	if v, ok := zones.Load(name); ok {
		return v.(*time.Location), nil
	}
	r, err := zip.NewReader(bytes.NewReader(zoneData), int64(len(zoneData)))
	if err != nil {
		return nil, err
	}
	for _, f := range r.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
			loc, err := time.LoadLocationFromTZData(name, data)
			if err != nil {
				return nil, err
			}
			zones.Store(name, loc)
			return loc, nil
		}
	}
	return nil, ErrPolicy
}

// ResolveCivil chooses the first occurrence of a repeated wall time and the
// first valid instant after a nonexistent wall time (02:30 -> 03:00 at a gap).
// Inspect explicit zone intervals instead of time.Date's unspecified DST choice.
func ResolveCivil(zone string, year int, month time.Month, day, hour, minute, second int) (time.Time, error) {
	loc, err := location(zone)
	if err != nil {
		return time.Time{}, err
	}
	wall := time.Date(year, month, day, hour, minute, second, 0, time.UTC)
	if year < 1969 || year > 9999 || wall.Year() != year || wall.Month() != month || wall.Day() != day || wall.Hour() != hour || wall.Minute() != minute || wall.Second() != second {
		return time.Time{}, ErrPolicy
	}
	lower, upper := wall.Add(-48*time.Hour), wall.Add(48*time.Hour)
	var match, gap, gapWall time.Time
	for cursor := lower; cursor.Before(upper); {
		local := cursor.In(loc)
		_, offset := local.Zone()
		zs, ze := local.ZoneBounds()
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		if (zs.IsZero() || !candidate.Before(zs)) && (ze.IsZero() || candidate.Before(ze)) && (match.IsZero() || candidate.Before(match)) {
			match = candidate
		}
		if !zs.IsZero() {
			startWall := zs.UTC().Add(time.Duration(offset) * time.Second)
			if startWall.After(wall) && (gapWall.IsZero() || startWall.Before(gapWall)) {
				gapWall, gap = startWall, zs.UTC()
			}
		}
		if ze.IsZero() || !ze.Before(upper) {
			break
		}
		if !ze.After(cursor) {
			return time.Time{}, ErrPolicy
		}
		cursor = ze
	}
	if !match.IsZero() {
		return match.UTC(), nil
	}
	if !gap.IsZero() {
		return gap.UTC(), nil
	}
	return time.Time{}, ErrPolicy
}

func boundary(r Monthly, month time.Time) (time.Time, error) {
	last := time.Date(month.Year(), month.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	day := r.Day
	if day > last {
		day = last
	}
	return ResolveCivil(r.Timezone, month.Year(), month.Month(), day, r.Hour, r.Minute, r.Second)
}

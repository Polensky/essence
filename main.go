package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

//go:embed static/*
var staticFiles embed.FS

const (
	dataURL     = "https://regieessencequebec.ca/data/stations-20260402132004.xlsx"
	defaultPort = "8080"
)

type Station struct {
	Name       string  `json:"name"`
	Brand      string  `json:"brand"`
	Address    string  `json:"address"`
	Region     string  `json:"region"`
	PostalCode string  `json:"postalCode"`
	Lat        float64 `json:"lat"`
	Lng        float64 `json:"lng"`
	Regular    float64 `json:"regular"`
	Super      float64 `json:"super"`
	Diesel     float64 `json:"diesel"`
}

type StationsResponse struct {
	LastUpdated string    `json:"lastUpdated"`
	Stations    []Station `json:"stations"`
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("static files: %v", err)
	}

	http.HandleFunc("/api/stations", handleStations)
	http.Handle("/", http.FileServer(http.FS(staticSub)))

	log.Printf("Listening on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleStations(w http.ResponseWriter, r *http.Request) {
	stations, err := fetchAndParse()
	if err != nil {
		http.Error(w, fmt.Sprintf("error: %v", err), http.StatusInternalServerError)
		return
	}

	resp := StationsResponse{
		LastUpdated: parseTimestampFromURL(dataURL),
		Stations:    stations,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	json.NewEncoder(w).Encode(resp)
}

func fetchAndParse() ([]Station, error) {
	log.Println("Fetching Excel data...")
	resp, err := http.Get(dataURL)
	if err != nil {
		return nil, fmt.Errorf("fetching data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "stations-*.xlsx")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("writing temp file: %w", err)
	}
	tmp.Close()

	f, err := excelize.OpenFile(tmp.Name())
	if err != nil {
		return nil, fmt.Errorf("opening excel: %w", err)
	}
	defer f.Close()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, fmt.Errorf("no sheets found")
	}

	rows, err := f.GetRows(sheets[0])
	if err != nil {
		return nil, fmt.Errorf("reading rows: %w", err)
	}

	if len(rows) < 2 {
		return nil, fmt.Errorf("not enough rows")
	}

	var stations []Station
	for _, row := range rows[1:] {
		if len(row) < 8 {
			continue
		}

		lat, err := strconv.ParseFloat(row[5], 64)
		if err != nil {
			continue
		}
		lng, err := strconv.ParseFloat(row[6], 64)
		if err != nil {
			continue
		}

		regular := parsePrice(row[7])
		if regular <= 0 {
			continue
		}

		s := Station{
			Name:       row[0],
			Brand:      row[1],
			Address:    row[2],
			Region:     row[3],
			PostalCode: row[4],
			Lat:        lat,
			Lng:        lng,
			Regular:    regular,
		}

		if len(row) > 8 {
			s.Super = parsePrice(row[8])
		}
		if len(row) > 9 {
			s.Diesel = parsePrice(row[9])
		}

		stations = append(stations, s)
	}

	log.Printf("Parsed %d stations", len(stations))
	return stations, nil
}

// parsePrice converts "190.9¢" to 190.9
func parsePrice(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/D" {
		return 0
	}
	s = strings.TrimSuffix(s, "¢")
	s = strings.TrimSuffix(s, "\u00a2") // cent sign
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

var tsRegex = regexp.MustCompile(`(\d{14})`)

// parseTimestampFromURL extracts a YYYYMMDDHHmmSS timestamp from the URL
// filename and returns it as a human-readable string.
func parseTimestampFromURL(rawURL string) string {
	base := path.Base(rawURL)
	match := tsRegex.FindString(base)
	if match == "" {
		return ""
	}

	loc, err := time.LoadLocation("America/Montreal")
	if err != nil {
		loc = time.UTC
	}

	t, err := time.ParseInLocation("20060102150405", match, loc)
	if err != nil {
		return ""
	}

	return t.Format(time.RFC3339)
}

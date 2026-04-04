package main

import (
	"compress/gzip"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed static/*
var staticFiles embed.FS

const (
	geojsonURL   = "https://regieessencequebec.ca/stations.geojson.gz"
	defaultPort  = "8080"
	pollInterval = 5 * time.Minute
)

// GeoJSON structures matching the upstream format.
type GeoJSONResponse struct {
	Type     string          `json:"type"`
	Metadata *GeoJSONMeta    `json:"metadata,omitempty"`
	Features json.RawMessage `json:"features"`
}

type GeoJSONMeta struct {
	GeneratedAt    string `json:"generated_at"`
	ExcelURL       string `json:"excel_url"`
	TotalStations  int    `json:"total_stations"`
	ExcelSizeBytes int    `json:"excel_size_bytes"`
}

// Station is our simplified JSON shape for the frontend.
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

// Snapshot holds the aggregated statistics for one fetch.
type Snapshot struct {
	GeneratedAt  string  `json:"generatedAt"`
	FetchedAt    string  `json:"fetchedAt"`
	RegularAvg   float64 `json:"regularAvg"`
	RegularMin   float64 `json:"regularMin"`
	RegularMax   float64 `json:"regularMax"`
	SuperAvg     float64 `json:"superAvg"`
	DieselAvg    float64 `json:"dieselAvg"`
	StationCount int     `json:"stationCount"`
}

type StatsResponse struct {
	Snapshots []Snapshot `json:"snapshots"`
}

// In-memory cache
var (
	cacheMu    sync.RWMutex
	cachedResp *StationsResponse
)

var db *sql.DB

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	dbPath := os.Getenv("ESSENCE_DB")
	if dbPath == "" {
		dbPath = "./essence.db"
	}

	var err error
	db, err = initDB(dbPath)
	if err != nil {
		log.Fatalf("db init: %v", err)
	}
	defer db.Close()

	// Initial fetch, then background poll every 5 minutes.
	go poller()

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("static files: %v", err)
	}

	http.HandleFunc("/api/stations", handleStations)
	http.HandleFunc("/api/stats", handleStats)
	http.HandleFunc("/api/regions", handleRegions)
	http.HandleFunc("/api/stats/region", handleRegionStats)
	http.HandleFunc("/api/station-deltas", handleStationDeltas)
	http.Handle("/", http.FileServer(http.FS(staticSub)))

	log.Printf("Listening on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// initDB opens (or creates) the SQLite database and ensures the schema exists.
func initDB(path string) (*sql.DB, error) {
	d, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	_, err = d.Exec(`
		CREATE TABLE IF NOT EXISTS snapshots (
			generated_at  TEXT PRIMARY KEY,
			fetched_at    TEXT NOT NULL,
			regular_avg   REAL,
			regular_min   REAL,
			regular_max   REAL,
			super_avg     REAL,
			diesel_avg    REAL,
			station_count INTEGER
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("create table: %w", err)
	}

	_, err = d.Exec(`
		CREATE TABLE IF NOT EXISTS region_snapshots (
			generated_at  TEXT NOT NULL,
			region        TEXT NOT NULL,
			regular_avg   REAL,
			regular_min   REAL,
			regular_max   REAL,
			super_avg     REAL,
			diesel_avg    REAL,
			station_count INTEGER,
			PRIMARY KEY (generated_at, region)
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("create region_snapshots table: %w", err)
	}

	_, err = d.Exec(`
		CREATE TABLE IF NOT EXISTS station_prices (
			address      TEXT NOT NULL,
			generated_at TEXT NOT NULL,
			regular      REAL,
			super        REAL,
			diesel       REAL,
			PRIMARY KEY (address, generated_at)
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("create station_prices table: %w", err)
	}

	// Keep only the last 48 hours of per-station price history to bound table size.
	_, err = d.Exec(`
		CREATE INDEX IF NOT EXISTS idx_station_prices_generated_at
		ON station_prices (generated_at)
	`)
	if err != nil {
		return nil, fmt.Errorf("create station_prices index: %w", err)
	}

	return d, nil
}

// poller fetches immediately then every pollInterval.
func poller() {
	fetchAndStore()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		fetchAndStore()
	}
}

// fetchAndStore fetches upstream data, updates the in-memory cache, and
// persists a snapshot to SQLite if the data has a new generated_at value.
func fetchAndStore() {
	resp, err := fetchAndParse()
	if err != nil {
		log.Printf("fetch error: %v", err)
		return
	}

	cacheMu.Lock()
	cachedResp = resp
	cacheMu.Unlock()

	if resp.LastUpdated == "" {
		return
	}

	// Compute and persist global aggregate.
	snap := computeSnapshot(resp)

	_, err = db.Exec(`
		INSERT OR IGNORE INTO snapshots
			(generated_at, fetched_at, regular_avg, regular_min, regular_max, super_avg, diesel_avg, station_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.GeneratedAt,
		snap.FetchedAt,
		snap.RegularAvg,
		snap.RegularMin,
		snap.RegularMax,
		snap.SuperAvg,
		snap.DieselAvg,
		snap.StationCount,
	)
	if err != nil {
		log.Printf("db insert error: %v", err)
	}

	// Compute and persist per-region aggregates.
	regionSnaps := computeRegionSnapshots(resp)
	for _, rs := range regionSnaps {
		_, err = db.Exec(`
			INSERT OR IGNORE INTO region_snapshots
				(generated_at, region, regular_avg, regular_min, regular_max, super_avg, diesel_avg, station_count)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			rs.GeneratedAt,
			rs.Region,
			rs.RegularAvg,
			rs.RegularMin,
			rs.RegularMax,
			rs.SuperAvg,
			rs.DieselAvg,
			rs.StationCount,
		)
		if err != nil {
			log.Printf("db region insert error: %v", err)
		}
	}

	// Persist per-station prices for 24h delta calculations.
	for _, s := range resp.Stations {
		_, err = db.Exec(`
			INSERT OR IGNORE INTO station_prices (address, generated_at, regular, super, diesel)
			VALUES (?, ?, ?, ?, ?)`,
			s.Address,
			resp.LastUpdated,
			s.Regular,
			s.Super,
			s.Diesel,
		)
		if err != nil {
			log.Printf("db station_prices insert error: %v", err)
		}
	}

	// Prune station_prices rows older than 48 hours.
	cutoff := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	if _, err = db.Exec(`DELETE FROM station_prices WHERE generated_at < ?`, cutoff); err != nil {
		log.Printf("db station_prices prune error: %v", err)
	}
}

// computeSnapshot derives aggregate statistics from a StationsResponse.
func computeSnapshot(resp *StationsResponse) Snapshot {
	now := time.Now().UTC().Format(time.RFC3339)
	snap := Snapshot{
		GeneratedAt: resp.LastUpdated,
		FetchedAt:   now,
	}

	var regularSum, superSum, dieselSum float64
	var regularCount, superCount, dieselCount int
	snap.RegularMin = 1<<53 - 1
	snap.RegularMax = 0

	for _, s := range resp.Stations {
		if s.Regular > 0 {
			regularSum += s.Regular
			regularCount++
			if s.Regular < snap.RegularMin {
				snap.RegularMin = s.Regular
			}
			if s.Regular > snap.RegularMax {
				snap.RegularMax = s.Regular
			}
		}
		if s.Super > 0 {
			superSum += s.Super
			superCount++
		}
		if s.Diesel > 0 {
			dieselSum += s.Diesel
			dieselCount++
		}
	}

	snap.StationCount = len(resp.Stations)
	if regularCount > 0 {
		snap.RegularAvg = regularSum / float64(regularCount)
	} else {
		snap.RegularMin = 0
	}
	if superCount > 0 {
		snap.SuperAvg = superSum / float64(superCount)
	}
	if dieselCount > 0 {
		snap.DieselAvg = dieselSum / float64(dieselCount)
	}

	return snap
}

// RegionSnapshot holds aggregated statistics for one fetch scoped to a region.
type RegionSnapshot struct {
	GeneratedAt  string  `json:"generatedAt"`
	Region       string  `json:"region"`
	RegularAvg   float64 `json:"regularAvg"`
	RegularMin   float64 `json:"regularMin"`
	RegularMax   float64 `json:"regularMax"`
	SuperAvg     float64 `json:"superAvg"`
	DieselAvg    float64 `json:"dieselAvg"`
	StationCount int     `json:"stationCount"`
}

// computeRegionSnapshots derives per-region aggregate statistics from a StationsResponse.
func computeRegionSnapshots(resp *StationsResponse) []RegionSnapshot {
	type acc struct {
		regularSum, superSum, dieselSum       float64
		regularCount, superCount, dieselCount int
		regularMin, regularMax                float64
		stationCount                          int
	}

	byRegion := map[string]*acc{}
	for _, s := range resp.Stations {
		if s.Region == "" {
			continue
		}
		a, ok := byRegion[s.Region]
		if !ok {
			a = &acc{regularMin: 1<<53 - 1}
			byRegion[s.Region] = a
		}
		a.stationCount++
		if s.Regular > 0 {
			a.regularSum += s.Regular
			a.regularCount++
			if s.Regular < a.regularMin {
				a.regularMin = s.Regular
			}
			if s.Regular > a.regularMax {
				a.regularMax = s.Regular
			}
		}
		if s.Super > 0 {
			a.superSum += s.Super
			a.superCount++
		}
		if s.Diesel > 0 {
			a.dieselSum += s.Diesel
			a.dieselCount++
		}
	}

	snaps := make([]RegionSnapshot, 0, len(byRegion))
	for region, a := range byRegion {
		rs := RegionSnapshot{
			GeneratedAt:  resp.LastUpdated,
			Region:       region,
			StationCount: a.stationCount,
		}
		if a.regularCount > 0 {
			rs.RegularAvg = a.regularSum / float64(a.regularCount)
			rs.RegularMin = a.regularMin
			rs.RegularMax = a.regularMax
		}
		if a.superCount > 0 {
			rs.SuperAvg = a.superSum / float64(a.superCount)
		}
		if a.dieselCount > 0 {
			rs.DieselAvg = a.dieselSum / float64(a.dieselCount)
		}
		snaps = append(snaps, rs)
	}
	return snaps
}

func handleStations(w http.ResponseWriter, r *http.Request) {
	cacheMu.RLock()
	resp := cachedResp
	cacheMu.RUnlock()

	if resp == nil {
		http.Error(w, "data not yet available, please retry shortly", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	json.NewEncoder(w).Encode(resp)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	daysStr := r.URL.Query().Get("days")
	days := 7
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d > 0 {
			days = d
		}
	}

	// days=0 means all data
	var rows *sql.Rows
	var err error
	if daysStr == "0" {
		rows, err = db.Query(`
			SELECT generated_at, fetched_at, regular_avg, regular_min, regular_max,
			       super_avg, diesel_avg, station_count
			FROM snapshots
			ORDER BY generated_at ASC
		`)
	} else {
		since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
		rows, err = db.Query(`
			SELECT generated_at, fetched_at, regular_avg, regular_min, regular_max,
			       super_avg, diesel_avg, station_count
			FROM snapshots
			WHERE generated_at >= ?
			ORDER BY generated_at ASC
		`, since)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("db query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	snapshots := []Snapshot{}
	for rows.Next() {
		var s Snapshot
		if err := rows.Scan(&s.GeneratedAt, &s.FetchedAt, &s.RegularAvg, &s.RegularMin, &s.RegularMax,
			&s.SuperAvg, &s.DieselAvg, &s.StationCount); err != nil {
			http.Error(w, fmt.Sprintf("db scan: %v", err), http.StatusInternalServerError)
			return
		}
		snapshots = append(snapshots, s)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(StatsResponse{Snapshots: snapshots})
}

// handleRegions returns a sorted list of distinct region names from region_snapshots.
func handleRegions(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT DISTINCT region FROM region_snapshots ORDER BY region ASC`)
	if err != nil {
		http.Error(w, fmt.Sprintf("db query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	regions := []string{}
	for rows.Next() {
		var region string
		if err := rows.Scan(&region); err != nil {
			http.Error(w, fmt.Sprintf("db scan: %v", err), http.StatusInternalServerError)
			return
		}
		regions = append(regions, region)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(regions)
}

// handleRegionStats returns time-series snapshots for a specific region.
func handleRegionStats(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	if region == "" {
		http.Error(w, "missing region parameter", http.StatusBadRequest)
		return
	}

	daysStr := r.URL.Query().Get("days")
	days := 7
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d > 0 {
			days = d
		}
	}

	var rows *sql.Rows
	var err error
	if daysStr == "0" {
		rows, err = db.Query(`
			SELECT generated_at, regular_avg, regular_min, regular_max,
			       super_avg, diesel_avg, station_count
			FROM region_snapshots
			WHERE region = ?
			ORDER BY generated_at ASC
		`, region)
	} else {
		since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
		rows, err = db.Query(`
			SELECT generated_at, regular_avg, regular_min, regular_max,
			       super_avg, diesel_avg, station_count
			FROM region_snapshots
			WHERE region = ? AND generated_at >= ?
			ORDER BY generated_at ASC
		`, region, since)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("db query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	snapshots := []Snapshot{}
	for rows.Next() {
		var s Snapshot
		if err := rows.Scan(&s.GeneratedAt, &s.RegularAvg, &s.RegularMin, &s.RegularMax,
			&s.SuperAvg, &s.DieselAvg, &s.StationCount); err != nil {
			http.Error(w, fmt.Sprintf("db scan: %v", err), http.StatusInternalServerError)
			return
		}
		snapshots = append(snapshots, s)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(StatsResponse{Snapshots: snapshots})
}

// StationDelta holds the price change percentage for a station over the available
// history window. Fields are pointers so that missing data serialises as JSON null.
// ElapsedHours is the actual time span between the oldest and newest snapshot used.
type StationDelta struct {
	Regular      *float64 `json:"regular"`
	Super        *float64 `json:"super"`
	Diesel       *float64 `json:"diesel"`
	ElapsedHours float64  `json:"elapsedHours"`
}

// handleStationDeltas returns a map of address → StationDelta for all stations
// that have at least two distinct price records within the 48h retention window.
// The delta is computed between the oldest and newest available snapshot, and
// ElapsedHours carries the actual time span so the frontend can label it honestly.
func handleStationDeltas(w http.ResponseWriter, r *http.Request) {
	since := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)

	// Fetch all rows in the retention window, oldest first.
	rows, err := db.Query(`
		SELECT address, generated_at, regular, super, diesel
		FROM station_prices
		WHERE generated_at >= ?
		ORDER BY address ASC, generated_at ASC
	`, since)
	if err != nil {
		http.Error(w, fmt.Sprintf("db query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type priceRow struct {
		generatedAt string
		regular     float64
		super       float64
		diesel      float64
	}

	type addrRows struct {
		old *priceRow // first (oldest) row seen for this address
		cur *priceRow // last (newest) row seen for this address
	}
	byAddr := map[string]*addrRows{}

	for rows.Next() {
		var addr, genAt string
		var reg, sup, die float64
		if err := rows.Scan(&addr, &genAt, &reg, &sup, &die); err != nil {
			http.Error(w, fmt.Sprintf("db scan: %v", err), http.StatusInternalServerError)
			return
		}
		ar, ok := byAddr[addr]
		if !ok {
			ar = &addrRows{}
			byAddr[addr] = ar
		}
		row := &priceRow{generatedAt: genAt, regular: reg, super: sup, diesel: die}
		// First row seen becomes the baseline; every row updates current.
		if ar.old == nil {
			ar.old = row
		}
		ar.cur = row
	}

	pctChange := func(cur, old float64) *float64 {
		if old <= 0 || cur <= 0 {
			return nil
		}
		v := (cur - old) / old * 100
		return &v
	}

	result := make(map[string]StationDelta, len(byAddr))
	for addr, ar := range byAddr {
		// Need at least two distinct snapshots to compute a meaningful delta.
		if ar.old == nil || ar.cur == nil || ar.old.generatedAt == ar.cur.generatedAt {
			continue
		}
		oldT, err1 := time.Parse(time.RFC3339, ar.old.generatedAt)
		curT, err2 := time.Parse(time.RFC3339, ar.cur.generatedAt)
		if err1 != nil || err2 != nil {
			continue
		}
		result[addr] = StationDelta{
			Regular:      pctChange(ar.cur.regular, ar.old.regular),
			Super:        pctChange(ar.cur.super, ar.old.super),
			Diesel:       pctChange(ar.cur.diesel, ar.old.diesel),
			ElapsedHours: curT.Sub(oldT).Hours(),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	json.NewEncoder(w).Encode(result)
}

func fetchAndParse() (*StationsResponse, error) {
	log.Println("Fetching GeoJSON data from upstream...")

	req, err := http.NewRequest("GET", geojsonURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "essence-quebec-map/1.0")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	// Handle gzip if the response is compressed
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" || strings.HasSuffix(geojsonURL, ".gz") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close()
		reader = gz
	}

	var geojson GeoJSONResponse
	if err := json.NewDecoder(reader).Decode(&geojson); err != nil {
		return nil, fmt.Errorf("decoding geojson: %w", err)
	}

	// Parse features
	var features []struct {
		Geometry struct {
			Coordinates [2]float64 `json:"coordinates"`
		} `json:"geometry"`
		Properties struct {
			Name       string `json:"Name"`
			Brand      string `json:"brand"`
			Address    string `json:"Address"`
			PostalCode string `json:"PostalCode"`
			Region     string `json:"Region"`
			Prices     []struct {
				GasType     string  `json:"GasType"`
				Price       *string `json:"Price"`
				IsAvailable bool    `json:"IsAvailable"`
			} `json:"Prices"`
		} `json:"properties"`
	}

	if err := json.Unmarshal(geojson.Features, &features); err != nil {
		return nil, fmt.Errorf("parsing features: %w", err)
	}

	var stations []Station
	for _, f := range features {
		lng := f.Geometry.Coordinates[0]
		lat := f.Geometry.Coordinates[1]
		if lat == 0 && lng == 0 {
			continue
		}

		s := Station{
			Name:       f.Properties.Name,
			Brand:      f.Properties.Brand,
			Address:    f.Properties.Address,
			Region:     f.Properties.Region,
			PostalCode: f.Properties.PostalCode,
			Lat:        lat,
			Lng:        lng,
		}

		for _, p := range f.Properties.Prices {
			if p.Price == nil || !p.IsAvailable {
				continue
			}
			price := parsePrice(*p.Price)
			if price <= 0 {
				continue
			}
			switch p.GasType {
			case "Régulier":
				s.Regular = price
			case "Super":
				s.Super = price
			case "Diesel":
				s.Diesel = price
			}
		}

		if s.Regular <= 0 {
			continue
		}

		stations = append(stations, s)
	}

	lastUpdated := ""
	if geojson.Metadata != nil && geojson.Metadata.GeneratedAt != "" {
		lastUpdated = geojson.Metadata.GeneratedAt
	}

	log.Printf("Parsed %d stations (last updated: %s)", len(stations), lastUpdated)

	return &StationsResponse{
		LastUpdated: lastUpdated,
		Stations:    stations,
	}, nil
}

// parsePrice converts "190.9¢" to 190.9
func parsePrice(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/D" {
		return 0
	}
	s = strings.TrimSuffix(s, "¢")
	s = strings.TrimSuffix(s, "\u00a2") // cent sign

	var v float64
	_, err := fmt.Sscanf(s, "%f", &v)
	if err != nil {
		return 0
	}
	return v
}

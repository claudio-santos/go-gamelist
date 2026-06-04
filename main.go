package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/templates/*.html
var embeddedFiles embed.FS

//go:embed web/static/*
var staticFS embed.FS

const pageSize = 60

var (
	reEventHandlers = regexp.MustCompile(`(?is)\s+on[a-z0-9_-]+\s*=\s*(".*?"|'.*?'|[^\s>]+)`)
	reStyleAttrs    = regexp.MustCompile(`(?is)\s+style\s*=\s*(".*?"|'.*?'|[^\s>]+)`)
	reDangerousURLs = regexp.MustCompile(`(?is)\s+(href|src)\s*=\s*("(?:(?:javascript:|data:|vbscript:)[^"]*)"|'(?:(?:javascript:|data:|vbscript:)[^']*)'|(?:javascript:|data:|vbscript:)[^\s>]+)`)
	reTagStripper   = regexp.MustCompile(`(?s)<[^>]+>`)
	blockedTags     = []string{"script", "style", "iframe", "object", "embed", "svg", "math", "form", "textarea", "select", "button", "video", "audio"}
)

type Link struct {
	Name string
	URL  string
}

type Game struct {
	ID              string
	Name            string
	SortName        string
	Description     string
	Summary         string
	Developers      string
	Publishers      string
	Genres          string
	GenreList       []string
	Platforms       string
	PlatformList    []string
	ReleaseDate     string
	Completion      string
	PlayTime        string
	PlayCount       string
	CommunityScore  string
	CriticScore     string
	UserScore       string
	AgeRating       string
	Categories      string
	CategoryList    []string
	Favorite        string
	Hidden          string
	Features        string
	FeatureList     []string
	GameID          string
	InstallFolder   string
	InstallSize     string
	Installed       string
	Added           string
	LastPlayed      string
	DateModified    string
	RecentActivity  string
	Roms            string
	Links           string
	LinkList        []Link
	Manual          string
	Notes           string
	PluginID        string
	Sources         string
	SourceList      []string
	Regions         string
	RegionList      []string
	Series          string
	Tags            string
	TagList         []string
	Version         string
	SearchName      string
	SearchGenres    []string
	SearchPlatforms []string
	SearchCategories []string
	SearchFeatures  []string
	SearchSources   []string
	SearchRegions   []string
	SearchTags      []string
	SearchText      string
}

type StoreSnapshot struct {
	Games    []Game
	ByID     map[string]Game
	Genres   []string
	Statuses []string
	Sources  []string
}

type CSVStore struct {
	path        string
	logger      *log.Logger
	mu          sync.RWMutex
	lastModTime time.Time
	lastSize    int64
	snapshot    *StoreSnapshot
	lastLoadErr error
}

type GameService struct {
	store *CSVStore
}

type Filters struct {
	Query     string
	Genre     string
	Status    string
	Source    string
	Score     string
	Page      int
	View      string
	SortBy    string
	SortOrder string
}

type GameListResult struct {
	Filters       Filters
	FilterQuery   string
	Results       []Game
	Genres        []string
	Statuses      []string
	Sources       []string
	TotalCount    int
	FilteredCount int
	Page          int
	TotalPages    int
	HasPrev       bool
	HasNext       bool
	PrevPageURL   string
	NextPageURL   string
}

type HomePageData struct {
	Title string
	List  GameListResult
	View  string
}

type DetailPageData struct {
	Title   string
	Game    Game
	BackURL string
}

type App struct {
	service   *GameService
	templates *template.Template
	logger    *log.Logger
}

type Config struct {
	Port string
	CSV  string
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags)

	config, err := loadConfig("config.yaml")
	if err != nil {
		logger.Fatalf("failed to load config.yaml: %v", err)
	}

	addr := flag.String("addr", normalizeAddr(config.Port), "HTTP listen address")
	csvPath := flag.String("csv", config.CSV, "path to the CSV file")
	flag.Parse()

	store, err := NewCSVStore(*csvPath, logger)
	if err != nil {
		logger.Fatalf("failed to load initial CSV: %v", err)
	}

	templates, err := loadTemplates()
	if err != nil {
		logger.Fatalf("failed to load templates: %v", err)
	}

	app := &App{
		service:   &GameService{store: store},
		templates: templates,
		logger:    logger,
	}

	logger.Printf("server available at http://localhost%s", *addr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:    *addr,
		Handler: app.routes(),
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("HTTP server failed: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Print("shutting down server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Fatalf("server shutdown failed: %v", err)
	}
	logger.Print("server stopped")
}

func NewCSVStore(path string, logger *log.Logger) (*CSVStore, error) {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	snapshot, err := loadSnapshot(path)
	if err != nil {
		return nil, err
	}

	return &CSVStore{
		path:        path,
		logger:      logger,
		lastModTime: info.ModTime(),
		lastSize:    info.Size(),
		snapshot:    snapshot,
	}, nil
}

func (s *CSVStore) Snapshot() (*StoreSnapshot, error) {
	if err := s.ensureFresh(); err != nil {
		s.mu.RLock()
		snapshot := s.snapshot
		s.mu.RUnlock()
		if snapshot != nil {
			return snapshot, nil
		}
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.snapshot == nil {
		return nil, errors.New("snapshot unavailable")
	}
	return s.snapshot, nil
}

func (s *CSVStore) ensureFresh() error {
	info, err := os.Stat(s.path)
	if err != nil {
		s.logger.Printf("failed to stat CSV: %v", err)
		return err
	}

	s.mu.RLock()
	unchanged := info.ModTime().Equal(s.lastModTime) && info.Size() == s.lastSize
	s.mu.RUnlock()
	if unchanged {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if info.ModTime().Equal(s.lastModTime) && info.Size() == s.lastSize {
		return nil
	}

	snapshot, loadErr := loadSnapshot(s.path)
	s.lastModTime = info.ModTime()
	s.lastSize = info.Size()
	s.lastLoadErr = loadErr
	if loadErr != nil {
		s.logger.Printf("failed to reload CSV; keeping last valid snapshot: %v", loadErr)
		return loadErr
	}

	s.snapshot = snapshot
	return nil
}

func loadSnapshot(path string) (*StoreSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	header, err := reader.Read()
	if err != nil {
		return nil, err
	}
	if len(header) == 0 {
		return nil, errors.New("CSV has no header")
	}

	header[0] = strings.TrimPrefix(header[0], "\uFEFF")
	indexes := make(map[string]int, len(header))
	for i, name := range header {
		indexes[name] = i
	}

	for _, column := range []string{"Name", "Id"} {
		if _, ok := indexes[column]; !ok {
			return nil, fmt.Errorf("missing required column: %s", column)
		}
	}

	var games []Game
	genreSet := make(map[string]struct{})
	statusSet := make(map[string]struct{})
	sourceSet := make(map[string]struct{})
	byID := make(map[string]Game)

	for {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}

		game := Game{
			ID:             csvValue(record, indexes, "Id"),
			Name:           csvValue(record, indexes, "Name"),
			SortName:       csvValue(record, indexes, "Sorting Name"),
			Description:    sanitizeHTML(csvValue(record, indexes, "Description")),
			Developers:     csvValue(record, indexes, "Developers"),
			Publishers:     csvValue(record, indexes, "Publishers"),
			Genres:         csvValue(record, indexes, "Genres"),
			Platforms:      csvValue(record, indexes, "Platforms"),
			ReleaseDate:    csvValue(record, indexes, "Release Date"),
			Completion:     csvValue(record, indexes, "Completion Status"),
			PlayTime:       csvValue(record, indexes, "Time Played"),
			PlayCount:      csvValue(record, indexes, "Play Count"),
			CommunityScore: csvValue(record, indexes, "Community Score"),
			CriticScore:    csvValue(record, indexes, "Critic Score"),
			UserScore:      csvValue(record, indexes, "User Score"),
			AgeRating:      csvValue(record, indexes, "Age Rating"),
			Categories:     csvValue(record, indexes, "Categories"),
			Favorite:       csvValue(record, indexes, "Favorite"),
			Hidden:         csvValue(record, indexes, "Hidden"),
			Features:       csvValue(record, indexes, "Features"),
			GameID:         csvValue(record, indexes, "Game Id"),
			InstallFolder:  csvValue(record, indexes, "Installation Folder"),
			InstallSize:    csvValue(record, indexes, "Install Size"),
			Installed:      csvValue(record, indexes, "Installed"),
			Added:          csvValue(record, indexes, "Added"),
			LastPlayed:     csvValue(record, indexes, "Last Played"),
			DateModified:   csvValue(record, indexes, "Date Modified"),
			RecentActivity: csvValue(record, indexes, "Recent Activity"),
			Roms:           csvValue(record, indexes, "Roms"),
			Links:          csvValue(record, indexes, "Links"),
			Manual:         csvValue(record, indexes, "Manual"),
			Notes:          csvValue(record, indexes, "Notes"),
			PluginID:       csvValue(record, indexes, "PluginId"),
			Sources:        csvValue(record, indexes, "Sources"),
			Regions:        csvValue(record, indexes, "Regions"),
			Series:         csvValue(record, indexes, "Series"),
			Tags:           csvValue(record, indexes, "Tags"),
			Version:        csvValue(record, indexes, "Version"),
		}

		if game.ID == "" || game.Name == "" {
			continue
		}
		if game.SortName == "" {
			game.SortName = game.Name
		}

		game.GenreList = splitCSVList(game.Genres)
		game.PlatformList = splitCSVList(game.Platforms)
		game.CategoryList = splitCSVList(game.Categories)
		game.FeatureList = splitCSVList(game.Features)
		game.SourceList = splitCSVList(game.Sources)
		game.RegionList = splitCSVList(game.Regions)
		game.TagList = splitCSVList(game.Tags)
		game.LinkList = parseGameLinks(game.Links)
		game.SearchGenres = normalizeList(game.GenreList)
		game.SearchPlatforms = normalizeList(game.PlatformList)
		game.SearchCategories = normalizeList(game.CategoryList)
		game.SearchFeatures = normalizeList(game.FeatureList)
		game.SearchSources = normalizeList(game.SourceList)
		game.SearchRegions = normalizeList(game.RegionList)
		game.SearchTags = normalizeList(game.TagList)
		game.SearchName = normalizeText(game.Name)
		game.SearchText = game.SearchName + " " + normalizeText(game.Developers) + " " + normalizeText(game.Publishers) + " " + strings.Join(game.SearchGenres, " ")
		game.Summary = excerptText(stripTags(game.Description), 180)

		for _, genre := range game.GenreList {
			genreSet[genre] = struct{}{}
		}
		for _, source := range game.SourceList {
			sourceSet[source] = struct{}{}
		}
		if game.Completion != "" {
			statusSet[game.Completion] = struct{}{}
		}

		games = append(games, game)
		byID[game.ID] = game
	}

	slices.SortFunc(games, func(a, b Game) int {
		return strings.Compare(strings.ToLower(a.SortName), strings.ToLower(b.SortName))
	})

	return &StoreSnapshot{
		Games:    games,
		ByID:     byID,
		Genres:   sortedKeys(genreSet),
		Statuses: sortedKeys(statusSet),
		Sources:  sortedKeys(sourceSet),
	}, nil
}

func csvValue(record []string, indexes map[string]int, key string) string {
	index, ok := indexes[key]
	if !ok || index >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[index])
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	slices.SortFunc(keys, func(a, b string) int {
		return strings.Compare(strings.ToLower(a), strings.ToLower(b))
	})
	return keys
}

func splitCSVList(value string) []string {
	if value == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func parseGameLinks(links string) []Link {
	if links == "" {
		return nil
	}
	var result []Link
	for _, part := range strings.Split(links, ", ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx := strings.Index(part, ":"); idx > 0 {
			result = append(result, Link{
				Name: strings.TrimSpace(part[:idx]),
				URL:  strings.TrimSpace(part[idx+1:]),
			})
		}
	}
	return result
}

func normalizeList(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, normalizeText(value))
	}
	return out
}

func normalizeText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer(
		"á", "a", "à", "a", "ã", "a", "â", "a", "ä", "a",
		"é", "e", "è", "e", "ê", "e", "ë", "e",
		"í", "i", "ì", "i", "î", "i", "ï", "i",
		"ó", "o", "ò", "o", "õ", "o", "ô", "o", "ö", "o",
		"ú", "u", "ù", "u", "û", "u", "ü", "u",
		"ç", "c", "ñ", "n",
	)
	value = replacer.Replace(value)
	value = strings.Join(strings.Fields(value), " ")
	return value
}

func sanitizeHTML(input string) string {
	if input == "" {
		return ""
	}

	safe := input
	for _, tag := range blockedTags {
		containerPattern := regexp.MustCompile(`(?is)<` + tag + `\b[^>]*>.*?</` + tag + `>`)
		singlePattern := regexp.MustCompile(`(?is)<` + tag + `\b[^>]*?/?>`)
		safe = containerPattern.ReplaceAllString(safe, "")
		safe = singlePattern.ReplaceAllString(safe, "")
	}
	safe = reEventHandlers.ReplaceAllString(safe, "")
	safe = reStyleAttrs.ReplaceAllString(safe, "")
	safe = reDangerousURLs.ReplaceAllString(safe, "")
	return safe
}

func stripTags(value string) string {
	if value == "" {
		return ""
	}
	text := reTagStripper.ReplaceAllString(value, " ")
	text = html.UnescapeString(text)
	return strings.Join(strings.Fields(text), " ")
}

func excerptText(value string, limit int) string {
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit])) + "..."
}

func (s *GameService) List(filters Filters) (GameListResult, error) {
	snapshot, err := s.store.Snapshot()
	if err != nil {
		return GameListResult{}, err
	}

	query := normalizeText(filters.Query)
	genre := normalizeText(filters.Genre)
	status := normalizeText(filters.Status)
	source := normalizeText(filters.Source)
	scoreThreshold, _ := strconv.Atoi(filters.Score)

	results := make([]Game, 0, len(snapshot.Games))
	for _, game := range snapshot.Games {
		if query != "" && !strings.Contains(game.SearchText, query) {
			continue
		}
		if genre != "" && !containsNormalized(game.SearchGenres, genre) {
			continue
		}
		if source != "" && !containsNormalized(game.SearchSources, source) {
			continue
		}
		if status != "" && normalizeText(game.Completion) != status {
			continue
		}
		if scoreThreshold > 0 && gameScore(game) < scoreThreshold {
			continue
		}
		results = append(results, game)
	}

	if filters.SortBy != "" {
		slices.SortFunc(results, func(a, b Game) int {
			var cmp int
			switch filters.SortBy {
			case "name":
				cmp = strings.Compare(strings.ToLower(a.SortName), strings.ToLower(b.SortName))
			case "developer":
				cmp = strings.Compare(strings.ToLower(a.Developers), strings.ToLower(b.Developers))
			case "source":
				cmp = strings.Compare(strings.ToLower(a.Sources), strings.ToLower(b.Sources))
			case "genres":
				cmp = strings.Compare(strings.ToLower(a.Genres), strings.ToLower(b.Genres))
			case "release":
				cmp = strings.Compare(a.ReleaseDate, b.ReleaseDate)
			case "playtime":
				cmp = compareIntStrings(a.PlayTime, b.PlayTime)
			case "score":
				cmp = compareScoreStrings(a, b)
			}
			if filters.SortOrder == "desc" {
				cmp = -cmp
			}
			return cmp
		})
	}

	totalFiltered := len(results)
	totalPages := (totalFiltered + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	page := filters.Page
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * pageSize
	end := start + pageSize
	if end > totalFiltered {
		end = totalFiltered
	}
	if start > totalFiltered {
		start = totalFiltered
	}

	result := GameListResult{
		Filters:       filters,
		FilterQuery:   filters.Encode(),
		Results:       results[start:end],
		Genres:        snapshot.Genres,
		Statuses:      snapshot.Statuses,
		Sources:       snapshot.Sources,
		TotalCount:    len(snapshot.Games),
		FilteredCount: totalFiltered,
		Page:          page,
		TotalPages:    totalPages,
		HasPrev:       page > 1,
		HasNext:       page < totalPages,
	}
	result.PrevPageURL = filters.pageURL(page - 1)
	result.NextPageURL = filters.pageURL(page + 1)
	return result, nil
}

func (s *GameService) Get(id string) (Game, error) {
	snapshot, err := s.store.Snapshot()
	if err != nil {
		return Game{}, err
	}

	game, ok := snapshot.ByID[id]
	if !ok {
		return Game{}, os.ErrNotExist
	}
	return game, nil
}

func containsNormalized(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func compareIntStrings(a, b string) int {
	na, _ := strconv.Atoi(a)
	nb, _ := strconv.Atoi(b)
	if na < nb {
		return -1
	}
	if na > nb {
		return 1
	}
	return 0
}

func gameScore(g Game) int {
	if n, err := strconv.Atoi(g.CommunityScore); err == nil && n > 0 {
		return n
	}
	if n, err := strconv.Atoi(g.CriticScore); err == nil && n > 0 {
		return n
	}
	if n, err := strconv.Atoi(g.UserScore); err == nil && n > 0 {
		return n
	}
	return 0
}

func compareScoreStrings(a, b Game) int {
	sa := gameScore(a)
	sb := gameScore(b)
	if sa < sb {
		return -1
	}
	if sa > sb {
		return 1
	}
	return 0
}

func addCommas(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for i := len(s); i > 0; i -= 3 {
		start := i - 3
		if start < 0 {
			start = 0
		}
		parts = append([]string{s[start:i]}, parts...)
	}
	return strings.Join(parts, ",")
}

func fmtPlayTime(seconds string) string {
	if seconds == "" {
		return ""
	}
	n, err := strconv.Atoi(seconds)
	if err != nil || n <= 0 {
		return ""
	}
	mins := n / 60
	if mins == 0 {
		return ""
	}
	hours := mins / 60
	minutes := mins % 60
	if hours == 0 {
		return strconv.Itoa(minutes) + "m"
	}
	hoursStr := strconv.Itoa(hours)
	if len(hoursStr) > 3 {
		var parts []string
		for i := len(hoursStr); i > 0; i -= 3 {
			start := i - 3
			if start < 0 {
				start = 0
			}
			parts = append([]string{hoursStr[start:i]}, parts...)
		}
		hoursStr = strings.Join(parts, ",")
	}
	if minutes == 0 {
		return hoursStr + "h"
	}
	return hoursStr + "h " + strconv.Itoa(minutes) + "m"
}

func (f Filters) HasActive() bool {
	return strings.TrimSpace(f.Query) != "" || strings.TrimSpace(f.Genre) != "" || strings.TrimSpace(f.Status) != "" || strings.TrimSpace(f.Source) != "" || strings.TrimSpace(f.Score) != ""
}

func (f Filters) Encode() string {
	values := url.Values{}
	if trimmed := strings.TrimSpace(f.Query); trimmed != "" {
		values.Set("q", trimmed)
	}
	if trimmed := strings.TrimSpace(f.Genre); trimmed != "" {
		values.Set("genre", trimmed)
	}
	if trimmed := strings.TrimSpace(f.Status); trimmed != "" {
		values.Set("status", trimmed)
	}
	if trimmed := strings.TrimSpace(f.Source); trimmed != "" {
		values.Set("source", trimmed)
	}
	if trimmed := strings.TrimSpace(f.Score); trimmed != "" {
		values.Set("score", trimmed)
	}
	if f.Page > 1 {
		values.Set("page", strconv.Itoa(f.Page))
	}
	if f.View != "" {
		values.Set("view", f.View)
	}
	if f.SortBy != "" {
		values.Set("sort", f.SortBy)
		if f.SortOrder == "desc" {
			values.Set("order", f.SortOrder)
		}
	}
	return values.Encode()
}

func (f Filters) pageURL(page int) string {
	f.Page = page
	encoded := f.Encode()
	if encoded == "" {
		return "/games"
	}
	return "/games?" + encoded
}

func (f Filters) sortURL(column string) string {
	order := "asc"
	if f.SortBy == column && f.SortOrder == "asc" {
		order = "desc"
	}
	f.SortBy = column
	f.SortOrder = order
	encoded := f.Encode()
	if encoded == "" {
		return "/games"
	}
	return "/games?" + encoded
}

func filtersFromRequest(r *http.Request) Filters {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	view := strings.TrimSpace(r.URL.Query().Get("view"))
	if view == "" {
		if c, err := r.Cookie("view"); err == nil {
			view = c.Value
		}
	}
	sortBy := strings.TrimSpace(r.URL.Query().Get("sort"))
	sortOrder := strings.TrimSpace(r.URL.Query().Get("order"))
	if sortOrder != "asc" && sortOrder != "desc" {
		sortOrder = "asc"
	}
	return Filters{
		Query:     strings.TrimSpace(r.URL.Query().Get("q")),
		Genre:     strings.TrimSpace(r.URL.Query().Get("genre")),
		Status:    strings.TrimSpace(r.URL.Query().Get("status")),
		Source:    strings.TrimSpace(r.URL.Query().Get("source")),
		Score:     strings.TrimSpace(r.URL.Query().Get("score")),
		Page:      page,
		View:      view,
		SortBy:    sortBy,
		SortOrder: sortOrder,
	}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	staticRoot, _ := fs.Sub(staticFS, "web/static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticRoot))))
	mux.HandleFunc("/", a.handleHome)
	mux.HandleFunc("/games", a.handleGames)
	mux.HandleFunc("/games/", a.handleGameDetail)
	return mux
}

func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	filters := filtersFromRequest(r)
	list, err := a.service.List(filters)
	if err != nil {
		a.serverError(w, err)
		return
	}

	data := HomePageData{
		Title: "Go Gamelist",
		List:  list,
		View:  filters.View,
	}
	a.render(w, http.StatusOK, "home.html", data)
}

func (a *App) handleGames(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/games" {
		http.NotFound(w, r)
		return
	}

	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/?"+r.URL.RawQuery, http.StatusSeeOther)
		return
	}

	filters := filtersFromRequest(r)
	list, err := a.service.List(filters)
	if err != nil {
		a.serverError(w, err)
		return
	}

	tmpl := "cards.html"
	if filters.View == "table" {
		tmpl = "table.html"
	}
	a.render(w, http.StatusOK, tmpl, list)
}

func (a *App) handleGameDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/games/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	game, err := a.service.Get(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		a.serverError(w, err)
		return
	}

	filters := filtersFromRequest(r)
	backURL := "/games"
	if encoded := filters.Encode(); encoded != "" {
		backURL += "?" + encoded
	}

	data := DetailPageData{
		Title:   game.Name + " | Go Gamelist",
		Game:    game,
		BackURL: backURL,
	}
	a.render(w, http.StatusOK, "detail.html", data)
}

func (a *App) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		a.logger.Printf("failed to render template %s: %v", name, err)
	}
}

func (a *App) serverError(w http.ResponseWriter, err error) {
	a.logger.Printf("internal error: %v", err)
	http.Error(w, "Internal server error.", http.StatusInternalServerError)
}

func loadTemplates() (*template.Template, error) {
	return template.New("").
		Funcs(template.FuncMap{
			"safeHTML": func(value string) template.HTML { return template.HTML(value) },
			"gameURL": func(id, query string) string {
				if query == "" {
					return "/games/" + id
				}
				return "/games/" + id + "?" + query
			},
			"fmtPlayTime": fmtPlayTime,
			"sortURL": func(f Filters, column string) string { return f.sortURL(column) },
			"sortIndicator": func(f Filters, column string) string {
				if f.SortBy == column {
					if f.SortOrder == "asc" {
						return " \u25B2"
					}
					return " \u25BC"
				}
				return ""
			},
			"formatDate": func(value string) string {
				if value == "" {
					return ""
				}
				if i := strings.Index(value, " "); i != -1 {
					return value[:i]
				}
				return value
			},
		}).
		ParseFS(embeddedFiles, "web/templates/*.html")
}

func loadConfig(path string) (Config, error) {
	config := Config{Port: "8080", CSV: "mygames.csv"}

	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config, nil
		}
		return Config{}, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Config{}, fmt.Errorf("invalid line in %s: %q", path, line)
		}

		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)

		switch key {
		case "port":
			config.Port = value
		case "csv":
			config.CSV = value
		default:
			return Config{}, fmt.Errorf("unsupported key in %s: %s", path, key)
		}
	}

	if err := scanner.Err(); err != nil {
		return Config{}, err
	}

	if config.Port == "" {
		config.Port = "8080"
	}

	return config, nil
}

func normalizeAddr(port string) string {
	port = strings.TrimSpace(port)
	if port == "" {
		return ":8080"
	}
	if strings.HasPrefix(port, ":") {
		return port
	}
	return ":" + port
}

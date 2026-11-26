// Package repo is the gorm-backed persistence layer of AgentFlow. Production
// deployments support MySQL (team/multi-instance server) and embedded SQLite in
// WAL mode (local/desktop Tauri). Tests open an in-process sqlite database
// through the same gorm code paths. There is no second in-memory implementation.
package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agentflow/internal/domain"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var (
	ErrRunDecisionStale  = errors.New("run changed before the decision was committed")
	ErrRunLeaseHeld      = errors.New("run lease is held by another instance")
	ErrRunLeaseLost      = errors.New("run lease ownership or generation changed")
	ErrRunWorkspaceBound = errors.New("run workspace is already bound to another host path")
)

// deps 是所有领域 Repo 共享的依赖（repo 层的共享依赖只有 gorm 句柄）。
type deps struct {
	DB *gorm.DB
}

func (d *deps) isMySQL() bool {
	return d.DB != nil && d.DB.Dialector.Name() == "mysql"
}

// sqlJSONText returns a portable "extract text at JSON path" expression.
// MySQL needs the JSON_UNQUOTE wrapper; sqlite's json_extract already returns
// unquoted text for string values.
func (d *deps) sqlJSONText(path string) string {
	if d.isMySQL() {
		return "JSON_UNQUOTE(JSON_EXTRACT(payload, ?))"
	}
	return "json_extract(payload, ?)"
}

// sqlPayloadJSONText is the literal-path variant of sqlJSONText for SELECT
// projections that cannot bind the path as a parameter. path is the JSON path
// below "$." and must be a code constant, never user input.
func (d *deps) sqlPayloadJSONText(path string) string {
	if d.isMySQL() {
		return "JSON_UNQUOTE(JSON_EXTRACT(payload, '$." + path + "'))"
	}
	return "json_extract(payload, '$." + path + "')"
}

// sqlExecutionContextJSONText is the execution_context-column counterpart of
// sqlPayloadJSONText. path is the JSON path below "$." and must be a code
// constant, never user input.
func (d *deps) sqlExecutionContextJSONText(path string) string {
	if d.isMySQL() {
		return "JSON_UNQUOTE(JSON_EXTRACT(execution_context, '$." + path + "'))"
	}
	return "json_extract(execution_context, '$." + path + "')"
}

// locking returns the query with a row-lock clause on MySQL. sqlite serializes
// writers already (single connection pool), so no clause is needed there.
func (d *deps) locking(q *gorm.DB) *gorm.DB {
	if d.isMySQL() {
		return q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return q
}

// lockingSkipLocked returns the query with FOR UPDATE SKIP LOCKED on MySQL so
// concurrent claimers do not block behind each other on the head row.
func (d *deps) lockingSkipLocked(q *gorm.DB) *gorm.DB {
	if d.isMySQL() {
		return q.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
	}
	return q
}

// lockingDB applies FOR UPDATE when tx uses the MySQL dialect. Package-level
// helpers without a *deps receiver use this to stay portable on SQLite.
func lockingDB(tx *gorm.DB) *gorm.DB {
	if tx != nil && tx.Dialector != nil && tx.Dialector.Name() == "mysql" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}

// CatalogRepo owns the reusable definitions: agents, rules, skills, MCP
// servers, executors, dockerfiles, environments, sandboxes, workflows,
// repositories, sync config, schedule jobs and definition revisions.
type CatalogRepo struct {
	*deps
	projects      *ProjectsRepo
	notifications *NotificationsRepo
	*BotInteractionsRepo
}

// ProjectsRepo owns projects and their members.
type BusinessDomainsRepo struct {
	*deps
}

type ProjectsRepo struct {
	*deps
}

// IssuesRepo owns issues and issue comments.
type IssuesRepo struct {
	*deps
	mu       sync.Mutex
	projects *ProjectsRepo
	users    *UsersRepo
	tasks    *TasksRepo
}

// TasksRepo owns tasks (the cross-instance idempotency records).
type TasksRepo struct {
	*deps
}

// UsersRepo owns users and their saved credentials.
type UsersRepo struct {
	*deps
}

// TokensRepo owns API tokens.
type TokensRepo struct {
	*deps
}

// AssistantRepo owns Studio AI assistant conversations and scheduled prompts.
type AssistantRepo struct {
	*deps
	users         *UsersRepo
	notifications *NotificationsRepo
	*BotInteractionsRepo
}

// RunsRepo owns runs, their children (nodes/events/telemetry/logs/artifacts),
// run leases and workflow metrics.
type RunsRepo struct {
	*deps
	assistant     *AssistantRepo
	issues        *IssuesRepo
	catalog       *CatalogRepo
	notifications *NotificationsRepo
	*BotInteractionsRepo
}

// RunnersRepo owns the CLI runner registry, runner tasks and runner events.
type RunnersRepo struct {
	*deps
}

// DeliveryRepo owns MR delivery state and the delivery locking that follows.
type DeliveryRepo struct {
	*deps
	issues  *IssuesRepo
	tasks   *TasksRepo
	catalog *CatalogRepo
	runs    *RunsRepo
}

// AnalyticsRepo owns the read-only operational analytics view.
type AnalyticsRepo struct {
	*deps
	runs     *RunsRepo
	catalog  *CatalogRepo
	delivery *DeliveryRepo
	issues   *IssuesRepo
	tasks    *TasksRepo
	projects *ProjectsRepo
	insights *InsightsRepo
}

// Store 组合各领域 Repo。领域 Repo 以匿名指针嵌入（未导出类型）：包外仅能
// 通过提升的方法调用，与现有调用形态完全兼容（方法提升对未导出类型同样生效）。
type Store struct {
	*deps
	*CatalogRepo
	*ProjectsRepo
	*BusinessDomainsRepo
	*IssuesRepo
	*TasksRepo
	*UsersRepo
	*TokensRepo
	*AssistantRepo
	*RunsRepo
	*RunnersRepo
	*DeliveryRepo
	*AnalyticsRepo
	*InsightsRepo
	*PreviewsRepo
	*EngineInstancesRepo
	*RoomsRepo
	*SpacesRepo
	*KnowledgeRepo
	*FeedbackRepo
	*NotificationsRepo
	*BotInteractionsRepo
}

// newStore wires the shared deps and every cross-domain pointer. Domains have
// no cycles, so a single construction pass is enough.
func newStore(db *gorm.DB) *Store {
	d := &deps{DB: db}
	s := &Store{deps: d}
	s.ProjectsRepo = &ProjectsRepo{deps: d}
	s.BusinessDomainsRepo = &BusinessDomainsRepo{deps: d}
	s.UsersRepo = &UsersRepo{deps: d}
	s.BotInteractionsRepo = &BotInteractionsRepo{deps: d}
	s.TokensRepo = &TokensRepo{deps: d}
	s.AssistantRepo = &AssistantRepo{deps: d, users: s.UsersRepo}
	s.TasksRepo = &TasksRepo{deps: d}
	s.NotificationsRepo = &NotificationsRepo{deps: d, users: s.UsersRepo, projects: s.ProjectsRepo, interactions: s.BotInteractionsRepo}
	s.AssistantRepo.notifications = s.NotificationsRepo
	s.CatalogRepo = &CatalogRepo{deps: d, projects: s.ProjectsRepo, notifications: s.NotificationsRepo}
	s.IssuesRepo = &IssuesRepo{deps: d, projects: s.ProjectsRepo, users: s.UsersRepo, tasks: s.TasksRepo}
	s.NotificationsRepo.issues = s.IssuesRepo
	s.RunsRepo = &RunsRepo{deps: d, assistant: s.AssistantRepo, issues: s.IssuesRepo, catalog: s.CatalogRepo, notifications: s.NotificationsRepo}
	s.RunnersRepo = &RunnersRepo{deps: d}
	s.DeliveryRepo = &DeliveryRepo{deps: d, issues: s.IssuesRepo, tasks: s.TasksRepo, catalog: s.CatalogRepo, runs: s.RunsRepo}
	s.InsightsRepo = &InsightsRepo{deps: d}
	s.AnalyticsRepo = &AnalyticsRepo{deps: d, runs: s.RunsRepo, catalog: s.CatalogRepo, delivery: s.DeliveryRepo, issues: s.IssuesRepo, tasks: s.TasksRepo, projects: s.ProjectsRepo, insights: s.InsightsRepo}
	s.PreviewsRepo = &PreviewsRepo{deps: d}
	s.EngineInstancesRepo = &EngineInstancesRepo{deps: d}
	s.RoomsRepo = &RoomsRepo{deps: d}
	s.SpacesRepo = &SpacesRepo{deps: d}
	s.KnowledgeRepo = &KnowledgeRepo{deps: d}
	s.FeedbackRepo = &FeedbackRepo{deps: d}
	return s
}

// Open connects the store to MySQL or embedded SQLite (WAL mode).
//
// Resolution order:
//  1. Explicit dsn argument: sqlite://, sqlite:, file:, or :memory: opens SQLite;
//     any other non-empty DSN opens MySQL.
//  2. Environment variables when dsn is empty:
//     - AGENTFLOW_DB_DRIVER=sqlite -> opens AGENTFLOW_SQLITE_PATH in WAL mode.
//     - AGENTFLOW_DB_DRIVER=mysql  -> opens AGENTFLOW_MYSQL_DSN.
//     - AGENTFLOW_DB_DRIVER unset  -> uses AGENTFLOW_MYSQL_DSN if set, else
//       AGENTFLOW_SQLITE_PATH if set; refuses to start when neither is configured.
//
// Per AGENTS.md, Open performs only connection setup + gorm incremental DDL
// (no code-side auto-seeding or data migrations).
func Open(dsn string) (*Store, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn != "" {
		if isSQLiteDSN(dsn) {
			return openSQLite(dsn)
		}
		return openMySQL(dsn)
	}
	driver := strings.ToLower(strings.TrimSpace(os.Getenv("AGENTFLOW_DB_DRIVER")))
	mysqlDSN := strings.TrimSpace(os.Getenv("AGENTFLOW_MYSQL_DSN"))
	sqlitePath := strings.TrimSpace(os.Getenv("AGENTFLOW_SQLITE_PATH"))
	switch driver {
	case "sqlite":
		if sqlitePath == "" {
			return nil, errors.New("AGENTFLOW_SQLITE_PATH 未配置：SQLite 模式需要数据库文件路径")
		}
		return openSQLite(sqlitePath)
	case "mysql":
		if mysqlDSN == "" {
			return nil, errors.New("AGENTFLOW_MYSQL_DSN 未配置：MySQL 模式缺少 DSN 时拒绝启动")
		}
		return openMySQL(mysqlDSN)
	case "":
		if mysqlDSN != "" {
			if isSQLiteDSN(mysqlDSN) {
				return openSQLite(mysqlDSN)
			}
			return openMySQL(mysqlDSN)
		}
		if sqlitePath != "" {
			return openSQLite(sqlitePath)
		}
		return nil, errors.New("AGENTFLOW_MYSQL_DSN 未配置：缺少数据库配置时拒绝启动（服务端请设置 AGENTFLOW_MYSQL_DSN，本地/桌面模式请设置 AGENTFLOW_DB_DRIVER=sqlite 与 AGENTFLOW_SQLITE_PATH）")
	default:
		return nil, fmt.Errorf("unsupported AGENTFLOW_DB_DRIVER %q (want mysql or sqlite)", driver)
	}
}

// OpenSQLite opens a production SQLite store at the given file path or SQLite
// URI with WAL mode and concurrency-safe PRAGMAs enabled.
func OpenSQLite(pathOrDSN string) (*Store, error) {
	pathOrDSN = strings.TrimSpace(pathOrDSN)
	if pathOrDSN == "" {
		return nil, errors.New("sqlite path is required")
	}
	return openSQLite(pathOrDSN)
}

func isSQLiteDSN(dsn string) bool {
	lower := strings.ToLower(strings.TrimSpace(dsn))
	return lower == ":memory:" ||
		strings.HasPrefix(lower, "sqlite://") ||
		strings.HasPrefix(lower, "sqlite:") ||
		strings.HasPrefix(lower, "file:")
}

func openMySQL(dsn string) (*Store, error) {
	if !strings.Contains(dsn, "charset=") {
		if strings.Contains(dsn, "?") {
			dsn += "&charset=utf8mb4&collation=utf8mb4_unicode_ci"
		} else {
			dsn += "?charset=utf8mb4&collation=utf8mb4_unicode_ci"
		}
	}
	if !strings.Contains(dsn, "parseTime=") {
		dsn += "&parseTime=true&loc=UTC"
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{TranslateError: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("mysql open: %w", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(32)
		sqlDB.SetMaxIdleConns(8)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
		if err := sqlDB.Ping(); err != nil {
			return nil, fmt.Errorf("mysql ping: %w", err)
		}
	}
	s := newStore(db)
	// Incremental DDL only (missing columns/tables/indexes) via gorm's
	// migrator. Schema baselines, data migrations and seed data are NOT run
	// from code: they are plain SQL scripts under migrations/, applied
	// manually by the operator.
	if err := autoMigrateIncremental(s.DB); err != nil {
		s.Close()
		return nil, fmt.Errorf("auto migrate: %w", err)
	}
	return s, nil
}

// sqliteWALPragmas lists the production PRAGMAs applied to every SQLite store.
// Single-connection pooling (MaxOpenConns=1) serializes in-process writes cleanly
// to avoid SQLITE_BUSY, while WAL + busy_timeout=5000 keeps external readers
// (e.g. CLI tools or backup utilities) non-blocking and crash-safe.
var sqliteWALPragmas = []string{
	"PRAGMA journal_mode = WAL",
	"PRAGMA busy_timeout = 5000",
	"PRAGMA synchronous = NORMAL",
	"PRAGMA foreign_keys = ON",
	"PRAGMA cache_size = -64000",
	"PRAGMA temp_store = MEMORY",
}

func openSQLite(raw string) (*Store, error) {
	dsn, fsPath, isMemory, err := normalizeSQLiteDSN(raw)
	if err != nil {
		return nil, fmt.Errorf("sqlite dsn: %w", err)
	}
	if !isMemory && fsPath != "" {
		dir := filepath.Dir(fsPath)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("sqlite mkdir %s: %w", dir, err)
			}
		}
	}
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		TranslateError: true,
		Logger:         logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("sqlite db handle: %w", err)
	}
	// Serialize in-process writers through one pooled connection so concurrent
	// engine/runner/assistant goroutines never hit SQLITE_BUSY while WAL +
	// busy_timeout preserve crash safety and cross-process read concurrency.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("sqlite ping: %w", err)
	}
	for _, pragma := range sqliteWALPragmas {
		if err := db.Exec(pragma).Error; err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("sqlite %s: %w", pragma, err)
		}
	}
	s := newStore(db)
	// Incremental DDL only (same contract as MySQL): no auto-seeding or data
	// migrations inside repo.Open().
	if err := autoMigrateIncremental(s.DB); err != nil {
		s.Close()
		return nil, fmt.Errorf("sqlite auto migrate: %w", err)
	}
	return s, nil
}

// normalizeSQLiteDSN accepts a filesystem path, :memory:, file: URI, sqlite://
// URI, or sqlite: URI and returns a glebarez/sqlite-compatible DSN plus the
// local filesystem path (empty for in-memory databases).
func normalizeSQLiteDSN(raw string) (dsn, fsPath string, isMemory bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false, errors.New("empty sqlite path")
	}
	lower := strings.ToLower(raw)
	if lower == ":memory:" {
		return ":memory:", "", true, nil
	}
	if strings.HasPrefix(lower, "file:") {
		rest := raw[len("file:"):]
		pathPart, query, _ := strings.Cut(rest, "?")
		isMemory = strings.EqualFold(pathPart, ":memory:") || strings.Contains(strings.ToLower(query), "mode=memory")
		if !isMemory && pathPart != "" {
			fsPath = cleanSQLiteFilePath(pathPart)
		}
		return appendSQLitePragmaParams(raw), fsPath, isMemory, nil
	}
	var pathPart, query string
	var hasQuery bool
	switch {
	case strings.HasPrefix(lower, "sqlite://"):
		pathPart, query, hasQuery = strings.Cut(raw[len("sqlite://"):], "?")
	case strings.HasPrefix(lower, "sqlite:"):
		pathPart, query, hasQuery = strings.Cut(raw[len("sqlite:"):], "?")
	default:
		pathPart = raw
	}
	if strings.EqualFold(strings.TrimSpace(pathPart), ":memory:") || strings.Contains(strings.ToLower(query), "mode=memory") {
		if hasQuery && query != "" {
			return appendSQLitePragmaParams("file:" + pathPart + "?" + query), "", true, nil
		}
		return ":memory:", "", true, nil
	}
	fsPath = cleanSQLiteFilePath(pathPart)
	if fsPath == "" {
		return "", "", false, fmt.Errorf("invalid sqlite path %q", raw)
	}
	uriPath := filepath.ToSlash(fsPath)
	base := "file:" + uriPath
	if hasQuery && query != "" {
		base += "?" + query
	}
	return appendSQLitePragmaParams(base), fsPath, false, nil
}

func cleanSQLiteFilePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	// Handle sqlite:///C:/path/to.db on Windows where URL parsing leaves a
	// leading slash before the drive letter.
	if len(p) >= 3 && p[0] == '/' && ((p[1] >= 'A' && p[1] <= 'Z') || (p[1] >= 'a' && p[1] <= 'z')) && p[2] == ':' {
		p = p[1:]
	}
	return filepath.Clean(filepath.FromSlash(p))
}

func appendSQLitePragmaParams(dsn string) string {
	lower := strings.ToLower(dsn)
	params := []struct{ key, expr string }{
		{"busy_timeout", "_pragma=busy_timeout(5000)"},
		{"journal_mode", "_pragma=journal_mode(WAL)"},
		{"synchronous", "_pragma=synchronous(NORMAL)"},
		{"foreign_keys", "_pragma=foreign_keys(ON)"},
		{"cache_size", "_pragma=cache_size(-64000)"},
		{"temp_store", "_pragma=temp_store(MEMORY)"},
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	var b strings.Builder
	b.WriteString(dsn)
	for _, p := range params {
		if strings.Contains(lower, p.key) {
			continue
		}
		b.WriteString(sep)
		b.WriteString(p.expr)
		sep = "&"
	}
	return b.String()
}

// Close releases the underlying connection pool. The gorm handle is kept so
// in-flight background goroutines fail gracefully ("sql: database is closed")
// instead of panicking on a nil handle.
func (s *Store) Close() {
	if s.DB == nil {
		return
	}
	if sqlDB, err := s.DB.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

func (s *Store) StorageInfo() map[string]any {
	driver := ""
	if s.DB != nil && s.DB.Dialector != nil {
		driver = s.DB.Dialector.Name()
	}
	note := "所有状态都在 MySQL：项目、Issue、SOP、运行记录、助手对话和定时任务都直接读写数据库。"
	info := map[string]any{
		"driver":   driver,
		"database": true,
	}
	if driver == "sqlite" {
		note = "所有状态都在本地 SQLite (WAL) 数据库：项目、Issue、SOP、运行记录、助手对话和定时任务均持久化到本地单文件数据库。"
		info["journalMode"] = "WAL"
	}
	info["note"] = note
	return info
}

func IssueBoundToTracker(issue domain.Issue) bool {
	if issue.Source != "gongfeng" && issue.Source != "tapd" {
		return false
	}
	if issue.ExternalRef == nil {
		return false
	}
	return strings.TrimSpace(issue.ExternalRef.ExternalID) != "" || issue.ExternalRef.ExternalIID != 0
}

// RestoreTrackerBinding fills Source/ExternalRef from a still-bound snapshot, or
// from a stable local id (tapd-… / gf-…) after a save wiped the pointer.
func RestoreTrackerBinding(issue *domain.Issue, existing domain.Issue) {
	if issue == nil || IssueBoundToTracker(*issue) {
		return
	}
	if IssueBoundToTracker(existing) {
		issue.Source = existing.Source
		issue.ExternalRef = existing.ExternalRef
		if strings.TrimSpace(issue.ID) == "" {
			issue.ID = existing.ID
		}
		return
	}
	id := strings.TrimSpace(issue.ID)
	if id == "" {
		id = strings.TrimSpace(existing.ID)
		issue.ID = id
	}
	if extID, ok := strings.CutPrefix(id, "tapd-"); ok && strings.TrimSpace(extID) != "" {
		issue.Source = "tapd"
		ref := domain.ExternalRef{Source: "tapd", ExternalID: extID, ExternalType: "story"}
		if existing.ExternalRef != nil {
			ref.ExternalURL = existing.ExternalRef.ExternalURL
			ref.ExternalState = existing.ExternalRef.ExternalState
		}
		issue.ExternalRef = &ref
		return
	}
	if _, iid := parseStoredGongfengIssueID(id); iid > 0 {
		issue.Source = "gongfeng"
		if existing.ExternalRef != nil {
			issue.ExternalRef = existing.ExternalRef
			return
		}
		issue.ExternalRef = &domain.ExternalRef{Source: "gongfeng", ExternalIID: iid, ExternalType: "issue"}
	}
}

func parseStoredGongfengIssueID(id string) (repo string, iid int) {
	body := strings.TrimPrefix(strings.TrimSpace(id), "gf-")
	if body == "" || body == strings.TrimSpace(id) {
		return "", 0
	}
	i := strings.LastIndex(body, "-")
	if i <= 0 {
		return "", 0
	}
	n := 0
	for _, ch := range body[i+1:] {
		if ch < '0' || ch > '9' {
			return "", 0
		}
		n = n*10 + int(ch-'0')
	}
	if n <= 0 {
		return "", 0
	}
	return body[:i], n
}

func cloneRunNodes(nodes map[string]*domain.NodeRun) map[string]*domain.NodeRun {
	if len(nodes) == 0 {
		return nil
	}
	out := make(map[string]*domain.NodeRun, len(nodes))
	for id, node := range nodes {
		if node == nil {
			continue
		}
		cp := *node
		out[id] = &cp
	}
	return out
}

func cloneRun(run domain.Run) domain.Run {
	run.Nodes = cloneRunNodes(run.Nodes)
	run.Events = append([]domain.RunEvent(nil), run.Events...)
	run.Telemetry = append([]domain.TelemetryEvent(nil), run.Telemetry...)
	run.Logs = append([]domain.LogRecord(nil), run.Logs...)
	run.Artifacts = append([]domain.Artifact(nil), run.Artifacts...)
	return run
}

func normalizeRefs(values []string, prefix string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimPrefix(v, prefix)
		v = strings.TrimSuffix(v, ".md")
		out = append(out, v)
	}
	return out
}

func sortSlice[T any](values []T, less func(a, b T) bool) {
	sort.Slice(values, func(i, j int) bool { return less(values[i], values[j]) })
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

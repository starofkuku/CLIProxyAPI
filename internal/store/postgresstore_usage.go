package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
)

type usageDetailRecord struct {
	DedupKey        string
	APIName         string
	APIKeyHash      string
	ModelName       string
	RequestedAt     time.Time
	Source          string
	AuthIndex       string
	Failed          bool
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CachedTokens    int64
	TotalTokens     int64
}

// AppendUsageRecord writes a single usage record directly into the detail table.
func (s *PostgresStore) AppendUsageRecord(ctx context.Context, record usage.PersistentRecord) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}

	apiName := strings.TrimSpace(record.APIName)
	if apiName == "" {
		return nil
	}
	modelName := strings.TrimSpace(record.ModelName)
	if modelName == "" {
		modelName = "unknown"
	}
	requestedAt := record.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now()
	}
	detail := usage.RequestDetail{
		Timestamp: requestedAt,
		Source:    record.Source,
		AuthIndex: record.AuthIndex,
		Failed:    record.Failed,
		Tokens:    normalizeUsageTokens(record.Tokens),
	}
	query := fmt.Sprintf(`
		INSERT INTO %s (
			dedup_key, api_name, api_key_hash, model_name, requested_at, source, auth_index, failed,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, NOW(), NOW())
		ON CONFLICT (dedup_key) DO NOTHING
	`, s.fullTableName(s.cfg.UsageDetailTable))
	_, err := s.db.ExecContext(
		ctx,
		query,
		usageDetailDedupKey(apiName, modelName, detail),
		apiName,
		record.APIKeyHash,
		modelName,
		requestedAt,
		record.Source,
		record.AuthIndex,
		record.Failed,
		detail.Tokens.InputTokens,
		detail.Tokens.OutputTokens,
		detail.Tokens.ReasoningTokens,
		detail.Tokens.CachedTokens,
		detail.Tokens.TotalTokens,
	)
	if err != nil {
		return fmt.Errorf("postgres store: append usage detail row: %w", err)
	}
	return nil
}

type UsageLegacyMigrationResult struct {
	Table         string
	DetailRows    int
	TotalRequests int64
}

type legacyUsageTableRef struct {
	schemaName string
	tableName  string
	qualified  string
	display    string
}

// PersistUsageSnapshot stores usage data as append-only detail rows instead of one large JSON blob.
func (s *PostgresStore) PersistUsageSnapshot(ctx context.Context, snapshot []byte) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}

	payload := strings.TrimSpace(string(snapshot))
	if payload == "" {
		return s.deleteUsageSnapshot(ctx)
	}

	var parsed usage.StatisticsSnapshot
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return fmt.Errorf("postgres store: parse usage snapshot: %w", err)
	}

	records := usageDetailRecordsFromSnapshot(parsed)
	if len(records) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres store: begin usage persistence transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	insertQuery := fmt.Sprintf(`
		INSERT INTO %s (
			dedup_key, api_name, api_key_hash, model_name, requested_at, source, auth_index, failed,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, NOW(), NOW())
		ON CONFLICT (dedup_key) DO NOTHING
	`, s.fullTableName(s.cfg.UsageDetailTable))
	for _, record := range records {
		if _, err = tx.ExecContext(
			ctx,
			insertQuery,
			record.DedupKey,
			record.APIName,
			record.APIKeyHash,
			record.ModelName,
			record.RequestedAt,
			record.Source,
			record.AuthIndex,
			record.Failed,
			record.InputTokens,
			record.OutputTokens,
			record.ReasoningTokens,
			record.CachedTokens,
			record.TotalTokens,
		); err != nil {
			return fmt.Errorf("postgres store: insert usage detail row: %w", err)
		}
	}

	if _, err = tx.ExecContext(
		ctx,
		fmt.Sprintf("DELETE FROM %s WHERE id = $1", s.fullTableName(s.cfg.UsageTable)),
		defaultUsageKey,
	); err != nil {
		return fmt.Errorf("postgres store: delete legacy usage snapshot: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("postgres store: commit usage persistence transaction: %w", err)
	}
	tx = nil
	return nil
}

// LoadUsageSnapshot reconstructs the serialized usage snapshot from detail rows.
func (s *PostgresStore) LoadUsageSnapshot(ctx context.Context) ([]byte, error) {
	return s.loadUsageSnapshot(ctx, time.Time{}, time.Time{}, false)
}

// LoadUsageSnapshotRange reconstructs a serialized usage snapshot from detail rows
// whose requested_at values fall in the half-open interval [start, end).
func (s *PostgresStore) LoadUsageSnapshotRange(ctx context.Context, start, end time.Time) ([]byte, error) {
	return s.loadUsageSnapshot(ctx, start, end, true)
}

func (s *PostgresStore) loadUsageSnapshot(ctx context.Context, start, end time.Time, filterByTime bool) ([]byte, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("postgres store: not initialized")
	}

	whereParts := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if filterByTime {
		if !start.IsZero() {
			args = append(args, start)
			whereParts = append(whereParts, fmt.Sprintf("requested_at >= $%d", len(args)))
		}
		if !end.IsZero() {
			args = append(args, end)
			whereParts = append(whereParts, fmt.Sprintf("requested_at < $%d", len(args)))
		}
	}
	whereClause := ""
	if len(whereParts) > 0 {
		whereClause = "WHERE " + strings.Join(whereParts, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT
			dedup_key, api_name, api_key_hash, model_name, requested_at, source, auth_index, failed,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens
		FROM %s
		%s
		ORDER BY requested_at, dedup_key
	`, s.fullTableName(s.cfg.UsageDetailTable), whereClause)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres store: load usage detail rows: %w", err)
	}
	defer rows.Close()

	records := make([]usageDetailRecord, 0, 128)
	for rows.Next() {
		var record usageDetailRecord
		if err = rows.Scan(
			&record.DedupKey,
			&record.APIName,
			&record.APIKeyHash,
			&record.ModelName,
			&record.RequestedAt,
			&record.Source,
			&record.AuthIndex,
			&record.Failed,
			&record.InputTokens,
			&record.OutputTokens,
			&record.ReasoningTokens,
			&record.CachedTokens,
			&record.TotalTokens,
		); err != nil {
			return nil, fmt.Errorf("postgres store: scan usage detail row: %w", err)
		}
		records = append(records, record)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres store: iterate usage detail rows: %w", err)
	}
	if len(records) == 0 {
		if filterByTime {
			hasDetails, errHasDetails := s.hasUsageDetailRows(ctx)
			if errHasDetails != nil {
				return nil, errHasDetails
			}
			if hasDetails {
				payload, errMarshal := json.Marshal(usageSnapshotFromDetailRecords(nil))
				if errMarshal != nil {
					return nil, fmt.Errorf("postgres store: marshal empty usage snapshot: %w", errMarshal)
				}
				return payload, nil
			}
		}
		raw, errLegacy := s.loadLegacyUsageSnapshot(ctx)
		if errLegacy != nil {
			return nil, errLegacy
		}
		if !filterByTime || len(strings.TrimSpace(string(raw))) == 0 {
			return raw, nil
		}
		var legacy usage.StatisticsSnapshot
		if errUnmarshal := json.Unmarshal(raw, &legacy); errUnmarshal != nil {
			return nil, fmt.Errorf("postgres store: parse legacy usage snapshot: %w", errUnmarshal)
		}
		filtered := usage.FilterSnapshotByTimeRange(legacy, start, end)
		payload, errMarshal := json.Marshal(filtered)
		if errMarshal != nil {
			return nil, fmt.Errorf("postgres store: marshal filtered legacy usage snapshot: %w", errMarshal)
		}
		return payload, nil
	}

	payload, err := json.Marshal(usageSnapshotFromDetailRecords(records))
	if err != nil {
		return nil, fmt.Errorf("postgres store: marshal usage snapshot: %w", err)
	}
	return payload, nil
}

func (s *PostgresStore) hasUsageDetailRows(ctx context.Context) (bool, error) {
	query := fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s LIMIT 1)", s.fullTableName(s.cfg.UsageDetailTable))
	var exists bool
	if err := s.db.QueryRowContext(ctx, query).Scan(&exists); err != nil {
		return false, fmt.Errorf("postgres store: check usage detail rows: %w", err)
	}
	return exists, nil
}

func (s *PostgresStore) loadLegacyUsageSnapshot(ctx context.Context) ([]byte, error) {
	query := fmt.Sprintf("SELECT content::text FROM %s WHERE id = $1", s.fullTableName(s.cfg.UsageTable))
	var payload string
	err := s.db.QueryRowContext(ctx, query, defaultUsageKey).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postgres store: load usage snapshot: %w", err)
	}
	return []byte(payload), nil
}

// MigrateLegacyUsageTable reads a legacy snapshot table and imports it into the detail-row storage.
func (s *PostgresStore) MigrateLegacyUsageTable(ctx context.Context, tableName string) (UsageLegacyMigrationResult, error) {
	result := UsageLegacyMigrationResult{}
	if s == nil || s.db == nil {
		return result, fmt.Errorf("postgres store: not initialized")
	}

	ref, err := s.resolveLegacyUsageTable(ctx, tableName)
	if err != nil {
		return result, err
	}
	result.Table = ref.display

	payload, err := s.loadLegacyUsageSnapshotFromTable(ctx, ref.qualified)
	if err != nil {
		return result, err
	}
	if len(strings.TrimSpace(string(payload))) == 0 {
		return result, nil
	}

	var snapshot usage.StatisticsSnapshot
	if err = json.Unmarshal(payload, &snapshot); err != nil {
		return result, fmt.Errorf("postgres store: parse legacy usage snapshot: %w", err)
	}
	if err = s.PersistUsageSnapshot(ctx, payload); err != nil {
		return result, err
	}

	result.TotalRequests = snapshot.TotalRequests
	result.DetailRows = len(usageDetailRecordsFromSnapshot(snapshot))
	return result, nil
}

func (s *PostgresStore) loadLegacyUsageSnapshotFromTable(ctx context.Context, qualifiedTable string) ([]byte, error) {
	var payload string
	queryWithID := fmt.Sprintf("SELECT content::text FROM %s WHERE id = $1 ORDER BY updated_at DESC LIMIT 1", qualifiedTable)
	err := s.db.QueryRowContext(ctx, queryWithID, defaultUsageKey).Scan(&payload)
	switch {
	case err == nil:
		return []byte(payload), nil
	case errors.Is(err, sql.ErrNoRows):
		queryFallback := fmt.Sprintf("SELECT content::text FROM %s ORDER BY updated_at DESC LIMIT 1", qualifiedTable)
		err = s.db.QueryRowContext(ctx, queryFallback).Scan(&payload)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err == nil {
			return []byte(payload), nil
		}
		if strings.Contains(err.Error(), `column "updated_at" does not exist`) {
			queryFallback = fmt.Sprintf("SELECT content::text FROM %s LIMIT 1", qualifiedTable)
			err = s.db.QueryRowContext(ctx, queryFallback).Scan(&payload)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			if err == nil {
				return []byte(payload), nil
			}
		}
	case strings.Contains(err.Error(), `column "id" does not exist`) || strings.Contains(err.Error(), `column "updated_at" does not exist`):
		queryFallback := fmt.Sprintf("SELECT content::text FROM %s LIMIT 1", qualifiedTable)
		err = s.db.QueryRowContext(ctx, queryFallback).Scan(&payload)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err == nil {
			return []byte(payload), nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("postgres store: load legacy usage snapshot from %s: %w", qualifiedTable, err)
	}
	return []byte(payload), nil
}

func (s *PostgresStore) resolveLegacyUsageTable(ctx context.Context, tableName string) (legacyUsageTableRef, error) {
	ref := legacyUsageTableRef{}
	resolved := strings.TrimSpace(tableName)
	if strings.Contains(resolved, ".") {
		parts := strings.Split(resolved, ".")
		if len(parts) != 2 {
			return ref, fmt.Errorf("postgres store: invalid legacy usage table name %q", tableName)
		}
		schemaName := strings.TrimSpace(parts[0])
		tableOnly := strings.TrimSpace(parts[1])
		if schemaName == "" || tableOnly == "" {
			return ref, fmt.Errorf("postgres store: invalid legacy usage table name %q", tableName)
		}
		return legacyUsageTableRef{
			schemaName: schemaName,
			tableName:  tableOnly,
			qualified:  quoteIdentifier(schemaName) + "." + quoteIdentifier(tableOnly),
			display:    schemaName + "." + tableOnly,
		}, nil
	}

	candidates := legacyUsageTableCandidates(resolved)
	for _, candidate := range candidates {
		foundRef, found, err := s.findLegacyUsageTableByName(ctx, candidate)
		if err != nil {
			return ref, err
		}
		if found {
			return foundRef, nil
		}
	}

	if len(candidates) == 1 {
		return ref, fmt.Errorf("postgres store: legacy usage table not found: %s", candidates[0])
	}
	return ref, fmt.Errorf("postgres store: legacy usage table not found; tried: %s", strings.Join(candidates, ", "))
}

func legacyUsageTableCandidates(tableName string) []string {
	resolved := strings.TrimSpace(tableName)
	switch strings.ToLower(resolved) {
	case "":
		return []string{"usage_store", "useage_storage", "usage_storage"}
	case "usage_store":
		return []string{"usage_store", "useage_storage", "usage_storage"}
	case "useage_storage":
		return []string{"useage_storage", "usage_store", "usage_storage"}
	case "usage_storage":
		return []string{"usage_storage", "usage_store", "useage_storage"}
	default:
		return []string{resolved}
	}
}

func (s *PostgresStore) findLegacyUsageTableByName(ctx context.Context, tableName string) (legacyUsageTableRef, bool, error) {
	ref := legacyUsageTableRef{}
	tableName = strings.TrimSpace(tableName)
	if tableName == "" {
		return ref, false, nil
	}

	preferredSchema := strings.TrimSpace(s.cfg.Schema)
	if preferredSchema != "" {
		exists, err := s.legacyUsageTableExists(ctx, preferredSchema, tableName)
		if err != nil {
			return ref, false, err
		}
		if exists {
			return legacyUsageTableRef{
				schemaName: preferredSchema,
				tableName:  tableName,
				qualified:  quoteIdentifier(preferredSchema) + "." + quoteIdentifier(tableName),
				display:    preferredSchema + "." + tableName,
			}, true, nil
		}
	}

	if preferredSchema != "public" {
		exists, err := s.legacyUsageTableExists(ctx, "public", tableName)
		if err != nil {
			return ref, false, err
		}
		if exists {
			return legacyUsageTableRef{
				schemaName: "public",
				tableName:  tableName,
				qualified:  quoteIdentifier("public") + "." + quoteIdentifier(tableName),
				display:    "public." + tableName,
			}, true, nil
		}
	}

	const query = `
		SELECT table_schema
		FROM information_schema.tables
		WHERE table_name = $1
		ORDER BY
			CASE
				WHEN table_schema = $2 THEN 0
				WHEN table_schema = 'public' THEN 1
				ELSE 2
			END,
			table_schema
		LIMIT 1
	`
	var schemaName string
	err := s.db.QueryRowContext(ctx, query, tableName, preferredSchema).Scan(&schemaName)
	if errors.Is(err, sql.ErrNoRows) {
		return ref, false, nil
	}
	if err != nil {
		return ref, false, fmt.Errorf("postgres store: find legacy usage table %s: %w", tableName, err)
	}
	return legacyUsageTableRef{
		schemaName: schemaName,
		tableName:  tableName,
		qualified:  quoteIdentifier(schemaName) + "." + quoteIdentifier(tableName),
		display:    schemaName + "." + tableName,
	}, true, nil
}

func (s *PostgresStore) legacyUsageTableExists(ctx context.Context, schemaName, tableName string) (bool, error) {
	const query = `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.tables
			WHERE table_schema = $1 AND table_name = $2
		)
	`
	var exists bool
	if err := s.db.QueryRowContext(ctx, query, schemaName, tableName).Scan(&exists); err != nil {
		return false, fmt.Errorf("postgres store: check legacy usage table %s.%s: %w", schemaName, tableName, err)
	}
	return exists, nil
}

func usageDetailRecordsFromSnapshot(snapshot usage.StatisticsSnapshot) []usageDetailRecord {
	records := make([]usageDetailRecord, 0, snapshot.TotalRequests)
	for apiName, apiSnapshot := range snapshot.APIs {
		apiName = strings.TrimSpace(apiName)
		if apiName == "" {
			continue
		}
		for modelName, modelSnapshot := range apiSnapshot.Models {
			modelName = strings.TrimSpace(modelName)
			if modelName == "" {
				modelName = "unknown"
			}
			for _, detail := range modelSnapshot.Details {
				if detail.Timestamp.IsZero() {
					detail.Timestamp = time.Now()
				}
				tokens := normalizeUsageTokens(detail.Tokens)
				records = append(records, usageDetailRecord{
					DedupKey:        usageDetailDedupKey(apiName, modelName, detail),
					APIName:         apiName,
					APIKeyHash:      "",
					ModelName:       modelName,
					RequestedAt:     detail.Timestamp,
					Source:          detail.Source,
					AuthIndex:       detail.AuthIndex,
					Failed:          detail.Failed,
					InputTokens:     tokens.InputTokens,
					OutputTokens:    tokens.OutputTokens,
					ReasoningTokens: tokens.ReasoningTokens,
					CachedTokens:    tokens.CachedTokens,
					TotalTokens:     tokens.TotalTokens,
				})
			}
		}
	}
	return records
}

func usageSnapshotFromDetailRecords(records []usageDetailRecord) usage.StatisticsSnapshot {
	snapshot := usage.StatisticsSnapshot{
		APIs:           make(map[string]usage.APISnapshot),
		RequestsByDay:  make(map[string]int64),
		RequestsByHour: make(map[string]int64),
		TokensByDay:    make(map[string]int64),
		TokensByHour:   make(map[string]int64),
	}

	for _, record := range records {
		apiName := strings.TrimSpace(record.APIName)
		if apiName == "" {
			continue
		}
		modelName := strings.TrimSpace(record.ModelName)
		if modelName == "" {
			modelName = "unknown"
		}

		tokens := usage.TokenStats{
			InputTokens:     record.InputTokens,
			OutputTokens:    record.OutputTokens,
			ReasoningTokens: record.ReasoningTokens,
			CachedTokens:    record.CachedTokens,
			TotalTokens:     record.TotalTokens,
		}
		tokens = normalizeUsageTokens(tokens)
		detail := usage.RequestDetail{
			Timestamp: record.RequestedAt,
			Source:    record.Source,
			AuthIndex: record.AuthIndex,
			Failed:    record.Failed,
			Tokens:    tokens,
		}

		snapshot.TotalRequests++
		if detail.Failed {
			snapshot.FailureCount++
		} else {
			snapshot.SuccessCount++
		}
		snapshot.TotalTokens += tokens.TotalTokens

		apiSnapshot, ok := snapshot.APIs[apiName]
		if !ok {
			apiSnapshot = usage.APISnapshot{Models: make(map[string]usage.ModelSnapshot)}
		}
		modelSnapshot := apiSnapshot.Models[modelName]
		modelSnapshot.TotalRequests++
		modelSnapshot.TotalTokens += tokens.TotalTokens
		modelSnapshot.Details = append(modelSnapshot.Details, detail)
		apiSnapshot.TotalRequests++
		apiSnapshot.TotalTokens += tokens.TotalTokens
		apiSnapshot.Models[modelName] = modelSnapshot
		snapshot.APIs[apiName] = apiSnapshot

		dayKey := detail.Timestamp.Format("2006-01-02")
		hourKey := fmt.Sprintf("%02d", detail.Timestamp.Hour()%24)
		snapshot.RequestsByDay[dayKey]++
		snapshot.RequestsByHour[hourKey]++
		snapshot.TokensByDay[dayKey] += tokens.TotalTokens
		snapshot.TokensByHour[hourKey] += tokens.TotalTokens
	}

	return snapshot
}

func usageDetailDedupKey(apiName, modelName string, detail usage.RequestDetail) string {
	timestamp := detail.Timestamp.UTC().Format(time.RFC3339Nano)
	tokens := normalizeUsageTokens(detail.Tokens)
	return fmt.Sprintf(
		"%s|%s|%s|%s|%s|%t|%d|%d|%d|%d|%d",
		apiName,
		modelName,
		timestamp,
		detail.Source,
		detail.AuthIndex,
		detail.Failed,
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
		tokens.TotalTokens,
	)
}

func normalizeUsageTokens(tokens usage.TokenStats) usage.TokenStats {
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}
	return tokens
}

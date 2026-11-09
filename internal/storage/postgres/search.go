package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// maxQueryTokens bounds the number of terms taken from a keyword query.
const maxQueryTokens = 8

// lexemeEscaper quotes the characters that are special inside a quoted
// tsquery lexeme. Tokens only ever contain letters and digits, so this is a
// second line of defence rather than a path that is normally taken.
var lexemeEscaper = strings.NewReplacer(`\`, `\\`, `'`, `''`)

// Search implements search.Repository. The page and the total are fetched in
// one round trip; the total is counted separately so that a page past the end
// still reports the real number of matches.
func (r *Repo) Search(ctx context.Context, q domain.SearchQuery) (domain.SearchResult, error) {
	f := newSearchFilter(q)
	perPage := q.PerPage
	if perPage < 1 {
		perPage = domain.DefaultPerPage
	}
	pageSQL := fmt.Sprintf("SELECT %s FROM contents%s ORDER BY %s LIMIT $%d OFFSET $%d",
		contentColumns, f.where(), orderBy(q, f.tsArg), len(f.args)+1, len(f.args)+2)
	pageArgs := append(slices.Clone(f.args), perPage, q.Offset())

	var result domain.SearchResult
	batch := &pgx.Batch{}
	batch.Queue("SELECT count(*) FROM contents"+f.where(), f.args...).QueryRow(func(row pgx.Row) error {
		return row.Scan(&result.Total)
	})
	batch.Queue(pageSQL, pageArgs...).Query(func(rows pgx.Rows) error {
		items, err := pgx.CollectRows(rows, scanContent)
		result.Items = items
		return err
	})
	if err := r.pool.SendBatch(ctx, batch).Close(); err != nil {
		return domain.SearchResult{}, fmt.Errorf("search contents: %w", err)
	}
	return result, nil
}

// GetByID implements search.Repository.
func (r *Repo) GetByID(ctx context.Context, id string) (domain.ScoredContent, error) {
	rows, err := r.pool.Query(ctx, "SELECT "+contentColumns+" FROM contents WHERE id = $1", id)
	if err != nil {
		return domain.ScoredContent{}, fmt.Errorf("get content %q: %w", id, err)
	}
	item, err := pgx.CollectOneRow(rows, scanContent)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ScoredContent{}, fmt.Errorf("get content %q: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return domain.ScoredContent{}, fmt.Errorf("get content %q: %w", id, err)
	}
	return item, nil
}

// searchFilter accumulates the WHERE conditions of a search and their
// positional arguments. User input only ever travels through args.
type searchFilter struct {
	conds []string
	args  []any
	// tsArg is the 1-based position of the tsquery argument, 0 without one.
	tsArg int
}

func newSearchFilter(q domain.SearchQuery) searchFilter {
	var f searchFilter
	if !q.IncludeDeleted {
		f.conds = append(f.conds, "deleted_at IS NULL")
	}
	if q.Type != nil {
		f.conds = append(f.conds, "type = "+f.add(string(*q.Type)))
	}
	if q.Provider != "" {
		f.conds = append(f.conds, "provider = "+f.add(q.Provider))
	}
	if ts := BuildTSQuery(q.Query); ts != "" {
		f.conds = append(f.conds, "search_vector @@ to_tsquery('simple', "+f.add(ts)+")")
		f.tsArg = len(f.args)
	}
	return f
}

// add appends v to the arguments and returns its placeholder, such as "$2".
func (f *searchFilter) add(v any) string {
	f.args = append(f.args, v)
	return "$" + strconv.Itoa(len(f.args))
}

// where renders the conditions, or "" when there are none.
func (f searchFilter) where() string {
	if len(f.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(f.conds, " AND ")
}

// orderBy renders the ORDER BY clause from the whitelisted sort fields. The
// direction is derived from q.Order and never copied from input, and every
// ordering ends with id so that pages are stable.
func orderBy(q domain.SearchQuery, tsArg int) string {
	dir := "DESC"
	if q.Order == domain.OrderAsc {
		dir = "ASC"
	}
	switch {
	case q.Sort == domain.SortRelevance && tsArg > 0:
		return fmt.Sprintf("ts_rank(search_vector, to_tsquery('simple', $%d)) %s, score DESC, id ASC", tsArg, dir)
	case q.Sort == domain.SortPublishedAt:
		return fmt.Sprintf("published_at %s, id ASC", dir)
	default:
		// Score ordering, also the fallback for relevance without a keyword query.
		return fmt.Sprintf("score %s, published_at DESC, id ASC", dir)
	}
}

// BuildTSQuery turns free text into a tsquery for the 'simple' configuration:
// the text is lower-cased and split on anything that is not a letter or a
// digit, at most maxQueryTokens tokens are kept, each becomes a quoted prefix
// term such as 'go':* and the terms are joined with &. It returns "" when no
// token remains, which callers treat as "match everything".
func BuildTSQuery(query string) string {
	tokens := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(tokens) > maxQueryTokens {
		tokens = tokens[:maxQueryTokens]
	}
	terms := make([]string, len(tokens))
	for i, tok := range tokens {
		terms[i] = "'" + lexemeEscaper.Replace(tok) + "':*"
	}
	return strings.Join(terms, " & ")
}

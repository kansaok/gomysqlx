package gomysqlx

import (
	"sort"
	"strings"
	"testing"

	"github.com/unoflavora/gomysqlx/ast"
)

// collectTableNames walks the AST and returns every named TableReference,
// mirroring how consumers (e.g. permission checkers) discover referenced tables.
func collectTableNames(node ast.Node) []string {
	seen := map[string]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if tr, ok := n.(*ast.TableReference); ok && tr.Name != "" && !strings.HasPrefix(tr.Name, "(") {
			seen[strings.ToLower(tr.Name)] = true
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// findDerivedSubqueries returns the Subquery of every derived TableReference.
func findDerivedSubqueries(node ast.Node) []ast.Statement {
	var subs []ast.Statement
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if tr, ok := n.(*ast.TableReference); ok && tr.Subquery != nil {
			subs = append(subs, tr.Subquery)
		}
		return true
	})
	return subs
}

func TestDerivedTable_SetOperations(t *testing.T) {
	tests := []struct {
		name       string
		sql        string
		wantTables []string
		wantSetOp  string // expected operator of the first derived subquery; "" = plain SELECT
	}{
		{
			name:       "FROM derived UNION ALL of literals",
			sql:        "SELECT t.a FROM (SELECT 1 AS a UNION ALL SELECT 2) t",
			wantTables: []string{},
			wantSetOp:  "UNION",
		},
		{
			name:       "FROM derived UNION of real tables",
			sql:        "SELECT u.id FROM (SELECT id FROM users UNION SELECT id FROM customers) AS u",
			wantTables: []string{"customers", "users"},
			wantSetOp:  "UNION",
		},
		{
			name:       "FROM derived chained UNION ALL",
			sql:        "SELECT x.id FROM (SELECT id FROM a UNION ALL SELECT id FROM b UNION ALL SELECT id FROM c) x",
			wantTables: []string{"a", "b", "c"},
			wantSetOp:  "UNION",
		},
		{
			name:       "FROM derived EXCEPT",
			sql:        "SELECT x.id FROM (SELECT id FROM a EXCEPT SELECT id FROM b) x",
			wantTables: []string{"a", "b"},
			wantSetOp:  "EXCEPT",
		},
		{
			name:       "JOIN derived UNION ALL",
			sql:        "SELECT o.id FROM orders o JOIN (SELECT id FROM users UNION ALL SELECT id FROM admins) u ON u.id = o.user_id",
			wantTables: []string{"admins", "orders", "users"},
			wantSetOp:  "UNION",
		},
		{
			name:       "LEFT JOIN derived UNION with WHERE in branches",
			sql:        "SELECT o.id FROM orders o LEFT JOIN (SELECT id FROM users WHERE active = 1 UNION SELECT id FROM admins WHERE active = 1) u ON u.id = o.user_id",
			wantTables: []string{"admins", "orders", "users"},
			wantSetOp:  "UNION",
		},
		{
			name:       "nested derived UNION inside derived UNION",
			sql:        "SELECT z.id FROM (SELECT y.id FROM (SELECT id FROM a UNION SELECT id FROM b) y UNION ALL SELECT id FROM c) z",
			wantTables: []string{"a", "b", "c"},
			wantSetOp:  "UNION",
		},
		{
			name: "INSERT ... SELECT FROM derived UNION ALL with NOT EXISTS",
			sql: `INSERT INTO approval_pattern (pattern, action_name, bu)
SELECT t.pattern, t.action_name, 'KS'
FROM (
    SELECT 'p1' AS pattern, 'a1' AS action_name
    UNION ALL SELECT 'p2', 'a2'
    UNION ALL SELECT 'p3', 'a3'
) t
WHERE NOT EXISTS (
    SELECT 1 FROM approval_pattern ap WHERE ap.action_name = t.action_name AND ap.bu = 'KS'
)`,
			wantTables: []string{"approval_pattern"},
			wantSetOp:  "UNION",
		},
		{
			// The INSERT target lives in InsertStatement.TableName (not a
			// TableReference node); both UNION branches must be visible.
			name:       "INSERT ... SELECT FROM derived UNION hides a second table",
			sql:        "INSERT INTO target (id) SELECT t.id FROM (SELECT id FROM src1 UNION SELECT id FROM secret_table) t",
			wantTables: []string{"secret_table", "src1"},
			wantSetOp:  "UNION",
		},
		{
			name:       "plain derived table still parses as SelectStatement",
			sql:        "SELECT t.a FROM (SELECT a FROM users) t",
			wantTables: []string{"users"},
			wantSetOp:  "",
		},
		{
			name:       "derived table with WITH",
			sql:        "SELECT t.id FROM (WITH c AS (SELECT id FROM users) SELECT id FROM c) t",
			wantTables: []string{"c", "users"},
			wantSetOp:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree, err := Parse(tt.sql)
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}
			defer ast.ReleaseAST(tree)
			if len(tree.Statements) != 1 {
				t.Fatalf("expected 1 statement, got %d", len(tree.Statements))
			}
			stmt := tree.Statements[0]

			got := collectTableNames(stmt)
			if strings.Join(got, ",") != strings.Join(tt.wantTables, ",") {
				t.Errorf("tables = %v, want %v", got, tt.wantTables)
			}

			subs := findDerivedSubqueries(stmt)
			if len(subs) == 0 {
				t.Fatal("expected at least one derived table subquery")
			}
			switch sq := subs[0].(type) {
			case *ast.SetOperation:
				if tt.wantSetOp == "" {
					t.Errorf("expected plain SelectStatement, got SetOperation %s", sq.Operator)
				} else if !strings.EqualFold(sq.Operator, tt.wantSetOp) {
					t.Errorf("operator = %q, want %q", sq.Operator, tt.wantSetOp)
				}
			case *ast.SelectStatement:
				if tt.wantSetOp != "" {
					t.Errorf("expected SetOperation %s, got SelectStatement", tt.wantSetOp)
				}
			default:
				t.Errorf("unexpected derived subquery type %T", sq)
			}
		})
	}
}

func TestDerivedTable_SetOperation_All(t *testing.T) {
	tree, err := Parse("SELECT t.a FROM (SELECT 1 AS a UNION ALL SELECT 2) t")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	defer ast.ReleaseAST(tree)
	subs := findDerivedSubqueries(tree.Statements[0])
	if len(subs) != 1 {
		t.Fatalf("expected 1 derived subquery, got %d", len(subs))
	}
	op, ok := subs[0].(*ast.SetOperation)
	if !ok {
		t.Fatalf("expected *ast.SetOperation, got %T", subs[0])
	}
	if !op.All {
		t.Error("expected All=true for UNION ALL")
	}
}

func TestDerivedTable_SetOperation_SQLRoundTrip(t *testing.T) {
	sqls := []string{
		"SELECT t.a FROM (SELECT a FROM x UNION ALL SELECT a FROM y) t",
		"SELECT o.id FROM orders o JOIN (SELECT id FROM users UNION SELECT id FROM admins) u ON u.id = o.user_id",
	}
	for _, sql := range sqls {
		tree, err := Parse(sql)
		if err != nil {
			t.Fatalf("Parse(%q) failed: %v", sql, err)
		}
		sqler, ok := tree.Statements[0].(interface{ SQL() string })
		if !ok {
			ast.ReleaseAST(tree)
			t.Fatalf("statement %T has no SQL() method", tree.Statements[0])
		}
		out := sqler.SQL()
		ast.ReleaseAST(tree)
		if !strings.Contains(strings.ToUpper(out), "UNION") {
			t.Errorf("rendered SQL lost UNION: %q", out)
		}
		// Rendered SQL must itself be parseable.
		tree2, err := Parse(out)
		if err != nil {
			t.Errorf("re-parse of rendered SQL %q failed: %v", out, err)
			continue
		}
		ast.ReleaseAST(tree2)
	}
}

func TestDerivedTable_SetOperation_Errors(t *testing.T) {
	bad := []string{
		"SELECT t.a FROM (SELECT 1 UNION) t",
		"SELECT t.a FROM (SELECT 1 UNION ALL) t",
		"SELECT t.a FROM (SELECT 1 UNION SELECT 2 t",
		"SELECT t.a FROM (UNION SELECT 1) t",
		"SELECT o.id FROM orders o JOIN (SELECT id FROM users UNION) u ON u.id = o.user_id",
	}
	for _, sql := range bad {
		if tree, err := Parse(sql); err == nil {
			ast.ReleaseAST(tree)
			t.Errorf("expected parse error for %q", sql)
		}
	}
}

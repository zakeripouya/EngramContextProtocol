package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// Queries behind the local dashboard and its approvals inbox (milestone M10).
// They are used only by the user's own CLI and dashboard process, never over the
// protocol, so they do not check client grants.

// GraphNode is a memory drawn in the graph explorer.
type GraphNode struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Namespace   string  `json:"namespace"`
	Content     string  `json:"content"`
	Strength    float64 `json:"strength"`
	Activations int     `json:"activations"`
	LastActive  string  `json:"lastActive,omitempty"`
	Service     string  `json:"service,omitempty"`
}

// Graph is a bounded view of the memory graph.
type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []Edge      `json:"edges"`
}

// GraphView returns the strongest and most used current memories (up to limit) in a
// namespace ("" for all) and the edges among them.
func (s *Store) GraphView(namespace string, limit int) (Graph, error) {
	if limit <= 0 || limit > 1000 {
		limit = 250
	}
	q := `SELECT id, kind, namespace, content, strength, activations, COALESCE(last_activated_at,''), COALESCE(service,'')
		FROM memories WHERE valid_to IS NULL`
	args := []any{}
	if namespace != "" {
		q += ` AND namespace = ?`
		args = append(args, namespace)
	}
	q += ` ORDER BY strength + 0.05 * activations DESC LIMIT ?`
	rows, err := s.db.Query(q, append(args, limit)...)
	if err != nil {
		return Graph{}, err
	}
	g := Graph{Nodes: []GraphNode{}, Edges: []Edge{}}
	ids := map[string]bool{}
	for rows.Next() {
		var n GraphNode
		if err := rows.Scan(&n.ID, &n.Kind, &n.Namespace, &n.Content, &n.Strength, &n.Activations, &n.LastActive, &n.Service); err != nil {
			rows.Close()
			return Graph{}, err
		}
		n.Content = Short(n.Content)
		g.Nodes = append(g.Nodes, n)
		ids[n.ID] = true
	}
	rows.Close()
	if len(ids) == 0 {
		return g, nil
	}
	er, err := s.db.Query(`SELECT src, dst, kind, weight FROM edges`)
	if err != nil {
		return Graph{}, err
	}
	defer er.Close()
	for er.Next() {
		var e Edge
		if err := er.Scan(&e.From, &e.To, &e.Kind, &e.Weight); err != nil {
			return Graph{}, err
		}
		if ids[e.From] && ids[e.To] {
			g.Edges = append(g.Edges, e)
		}
	}
	return g, er.Err()
}

// MemoryDetail is one memory with its edges, for the dashboard.
type MemoryDetail struct {
	Memory    Memory   `json:"memory"`
	Edges     []Edge   `json:"edges"`
	Neighbors []Memory `json:"neighbors"`
}

// Detail loads a memory, its edges and its neighbors.
func (s *Store) Detail(id string) (MemoryDetail, error) {
	m, err := scanMemory(s.db.QueryRow(`SELECT `+memoryColumns+` FROM memories WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return MemoryDetail{}, fmt.Errorf("no memory %q", id)
	} else if err != nil {
		return MemoryDetail{}, err
	}
	d := MemoryDetail{Memory: m, Edges: []Edge{}, Neighbors: []Memory{}}
	rows, err := s.db.Query(`SELECT src, dst, kind, weight FROM edges WHERE src = ? OR dst = ? ORDER BY weight DESC LIMIT 60`, id, id)
	if err != nil {
		return d, err
	}
	var other []string
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.From, &e.To, &e.Kind, &e.Weight); err != nil {
			rows.Close()
			return d, err
		}
		d.Edges = append(d.Edges, e)
		if e.From == id {
			other = append(other, e.To)
		} else {
			other = append(other, e.From)
		}
	}
	rows.Close()
	d.Neighbors, err = s.memoriesByID(other)
	return d, err
}

// ListMemories lists current memories, newest first, optionally filtered.
func (s *Store) ListMemories(namespace, kind, contains string, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q, args := `SELECT `+memoryColumns+` FROM memories WHERE valid_to IS NULL`, []any{}
	if namespace != "" {
		q += ` AND namespace = ?`
		args = append(args, namespace)
	}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	if contains != "" {
		q += ` AND content LIKE ? ESCAPE '\'`
		r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
		args = append(args, "%"+r.Replace(contains)+"%")
	}
	rows, err := s.db.Query(q+` ORDER BY created_at DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// InboxItem is something waiting for the user's approval.
type InboxItem struct {
	Kind    string   `json:"kind"` // distilled or conflict
	ID      string   `json:"id"`   // memory id (distilled) or "a|b" (conflict)
	Title   string   `json:"title"`
	Detail  string   `json:"detail"`
	At      string   `json:"at"`
	Sources []Memory `json:"sources,omitempty"` // what a distilled fact was derived from, or the two sides of a conflict
}

// Inbox lists distilled facts nobody has reviewed and unresolved conflicts.
func (s *Store) Inbox() ([]InboxItem, error) {
	out := []InboxItem{}
	rows, err := s.db.Query(`SELECT m.id, m.content, m.namespace, m.created_at FROM memories m
		LEFT JOIN reviews r ON r.memory_id = m.id
		WHERE m.service = 'sleep' AND m.valid_to IS NULL AND r.memory_id IS NULL ORDER BY m.created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	type row struct{ id, content, ns, at string }
	var distilled []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.content, &r.ns, &r.at); err != nil {
			rows.Close()
			return nil, err
		}
		distilled = append(distilled, r)
	}
	rows.Close()
	for _, r := range distilled {
		var src []string
		sr, err := s.db.Query(`SELECT dst FROM edges WHERE src = ? AND kind = 'derived'`, r.id)
		if err != nil {
			return nil, err
		}
		for sr.Next() {
			var id string
			if sr.Scan(&id) == nil {
				src = append(src, id)
			}
		}
		sr.Close()
		mems, err := s.memoriesByID(src)
		if err != nil {
			return nil, err
		}
		noun := "memories"
		if len(mems) == 1 {
			noun = "memory"
		}
		out = append(out, InboxItem{Kind: "distilled", ID: r.id, Title: r.content,
			Detail: fmt.Sprintf("Distilled during sleep in %s from %d %s. Keep it at full strength, or reject it to archive it.", r.ns, len(mems), noun),
			At:     r.at, Sources: mems})
	}
	cr, err := s.db.Query(`SELECT e.src, e.dst, COALESCE(e.updated_at, a.created_at) FROM edges e
		JOIN memories a ON a.id = e.src JOIN memories b ON b.id = e.dst
		WHERE e.kind = 'conflicts' AND a.valid_to IS NULL AND b.valid_to IS NULL`)
	if err != nil {
		return nil, err
	}
	type pair struct{ a, b, at string }
	var pairs []pair
	for cr.Next() {
		var p pair
		if err := cr.Scan(&p.a, &p.b, &p.at); err != nil {
			cr.Close()
			return nil, err
		}
		pairs = append(pairs, p)
	}
	cr.Close()
	for _, p := range pairs {
		mems, err := s.memoriesByID([]string{p.a, p.b})
		if err != nil || len(mems) != 2 {
			continue
		}
		out = append(out, InboxItem{Kind: "conflict", ID: p.a + "|" + p.b, Title: "Two memories disagree",
			Detail: "Keep the one that is right; the other is archived as superseded.", At: p.at, Sources: mems})
	}
	return out, nil
}

// ReviewDistilled approves (keeps, at least 0.6 strength) or rejects (archives) a distilled fact.
func (s *Store) ReviewDistilled(id string, approve bool) error {
	var service string
	if err := s.db.QueryRow(`SELECT COALESCE(service,'') FROM memories WHERE id = ?`, id).Scan(&service); err != nil {
		return fmt.Errorf("no memory %q", id)
	}
	if service != "sleep" {
		return fmt.Errorf("only distilled facts are reviewed here")
	}
	verdict := "rejected"
	if approve {
		verdict = "approved"
		if _, err := s.db.Exec(`UPDATE memories SET strength = max(strength, 0.6) WHERE id = ?`, id); err != nil {
			return err
		}
	} else if err := s.Archive(id, ""); err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO reviews(memory_id, verdict, at) VALUES (?,?,?)`, id, verdict, ts(Now())); err != nil {
		return err
	}
	return audit(s.db, "engram-dashboard", "review-"+verdict, "", id)
}

// ResolveConflict keeps one memory and archives the other as superseded by it.
func (s *Store) ResolveConflict(keep, drop string) error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM edges WHERE kind = 'conflicts' AND ((src = ? AND dst = ?) OR (src = ? AND dst = ?))`,
		keep, drop, drop, keep).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("those memories are not in conflict")
	}
	if err := s.Archive(drop, keep); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM edges WHERE kind = 'conflicts' AND ((src = ? AND dst = ?) OR (src = ? AND dst = ?))`,
		keep, drop, drop, keep); err != nil {
		return err
	}
	return audit(s.db, "engram-dashboard", "resolve-conflict", "", keep+" over "+drop)
}

// ClientActivity summarizes what one client did.
type ClientActivity struct {
	Client  string         `json:"client"`
	Grants  []Grant        `json:"grants"`
	Actions map[string]int `json:"actions"` // audit action → count over the last 30 days
	Last    string         `json:"last,omitempty"`
}

// Clients returns every paired client with its grants and recent activity.
func (s *Store) Clients() ([]ClientActivity, error) {
	grants, err := s.Grants("")
	if err != nil {
		return nil, err
	}
	by := map[string]*ClientActivity{}
	var order []string
	for _, g := range grants {
		c := by[g.Client]
		if c == nil {
			c = &ClientActivity{Client: g.Client, Actions: map[string]int{}}
			by[g.Client] = c
			order = append(order, g.Client)
		}
		c.Grants = append(c.Grants, g)
	}
	since := ts(Now().AddDate(0, 0, -30))
	for _, name := range order {
		c := by[name]
		rows, err := s.db.Query(`SELECT action, count(*), max(at) FROM audit WHERE client = ? AND at >= ? GROUP BY action`, name, since)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a, last string
			var n int
			if rows.Scan(&a, &n, &last) == nil {
				c.Actions[a] = n
				if last > c.Last {
					c.Last = last
				}
			}
		}
		rows.Close()
	}
	out := make([]ClientActivity, 0, len(order))
	for _, n := range order {
		out = append(out, *by[n])
	}
	return out, nil
}

// NamespaceCount is the number of current memories in a namespace.
type NamespaceCount struct {
	Namespace string `json:"namespace"`
	Memories  int    `json:"memories"`
}

// NamespaceCounts counts current memories per namespace.
func (s *Store) NamespaceCounts() ([]NamespaceCount, error) {
	rows, err := s.db.Query(`SELECT namespace, count(*) FROM memories WHERE valid_to IS NULL GROUP BY namespace ORDER BY count(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NamespaceCount{}
	for rows.Next() {
		var c NamespaceCount
		if err := rows.Scan(&c.Namespace, &c.Memories); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

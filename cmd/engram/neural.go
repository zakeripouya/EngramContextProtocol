package main

import (
	"math/rand"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// neuralNet is the little network drawn at the top of the setup screen. It runs
// the same idea as recall: a node fires, activation spreads to its neighbours
// damped at each hop, and everything decays. Imports fire it as they store things.
type neuralNet struct {
	grid    [][]rune
	nodes   [][2]int // row, col
	nodeAt  map[[2]int]int
	adj     [][]int
	edgeOf  map[[2]int][2]int // edge cell -> the two nodes it joins
	act     []float64
	pending []pulse
	rng     *rand.Rand
}

type pulse struct {
	node, wait int
	energy     float64
}

const netArt = `` +
	`    ●──────●              ●──────────●        ` + "\n" +
	`   ╱        ╲            ╱            ╲       ` + "\n" +
	`  ●     ●────●──────────●       ●──────●──●   ` + "\n" +
	`   ╲   ╱      ╲        ╱ ╲     ╱           ╲  ` + "\n" +
	`    ●─●        ●──────●   ●───●      ●──────● ` + "\n" +
	`      ╲       ╱        ╲         ╲  ╱         ` + "\n" +
	`       ●─────●          ●─────────●─●         `

func isEdgeRune(r rune) bool { return r == '─' || r == '│' || r == '╱' || r == '╲' }

func newNeuralNet(seed int64) *neuralNet {
	n := &neuralNet{nodeAt: map[[2]int]int{}, edgeOf: map[[2]int][2]int{}, rng: rand.New(rand.NewSource(seed))}
	for _, line := range strings.Split(netArt, "\n") {
		n.grid = append(n.grid, []rune(line))
	}
	for r, row := range n.grid {
		for c, ch := range row {
			if ch == '●' {
				n.nodeAt[[2]int{r, c}] = len(n.nodes)
				n.nodes = append(n.nodes, [2]int{r, c})
			}
		}
	}
	n.adj = make([][]int, len(n.nodes))
	n.act = make([]float64, len(n.nodes))
	// Follow the line characters out of each node to find the nodes it joins.
	for i, pos := range n.nodes {
		seen := map[[2]int]bool{pos: true}
		var path [][2]int
		var walk func(p [2]int)
		walk = func(p [2]int) {
			for dr := -1; dr <= 1; dr++ {
				for dc := -1; dc <= 1; dc++ {
					q := [2]int{p[0] + dr, p[1] + dc}
					if seen[q] || q[0] < 0 || q[0] >= len(n.grid) || q[1] < 0 || q[1] >= len(n.grid[q[0]]) {
						continue
					}
					ch := n.grid[q[0]][q[1]]
					if j, ok := n.nodeAt[q]; ok && j != i && len(path) > 0 {
						if !containsInt(n.adj[i], j) {
							n.adj[i] = append(n.adj[i], j)
						}
						for _, cell := range path {
							if _, done := n.edgeOf[cell]; !done {
								n.edgeOf[cell] = [2]int{i, j}
							}
						}
						continue
					}
					if isEdgeRune(ch) && connects(n.grid[p[0]][p[1]], ch, dr, dc) {
						seen[q] = true
						path = append(path, q)
						walk(q)
						path = path[:len(path)-1]
					}
				}
			}
		}
		walk(pos)
	}
	return n
}

// connects keeps the walk on one line: horizontal runs only step sideways,
// diagonals only along their slope.
func connects(from, to rune, dr, dc int) bool {
	ok := func(r rune) bool {
		switch r {
		case '─':
			return dr == 0
		case '│':
			return dc == 0
		case '╱':
			return dr == -dc
		case '╲':
			return dr == dc
		}
		return true // a node: any direction out
	}
	return ok(to) && (from == '●' || ok(from))
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// Fire starts a pulse at node i (random when i < 0).
func (n *neuralNet) Fire(i int, energy float64) {
	if len(n.nodes) == 0 {
		return
	}
	if i < 0 {
		i = n.rng.Intn(len(n.nodes))
	}
	n.pending = append(n.pending, pulse{node: i, energy: energy})
}

// Step advances one frame: decay, then deliver pulses whose delay is up, each
// spreading to its neighbours at 0.75 of its energy (like recall's damping).
func (n *neuralNet) Step() {
	for i := range n.act {
		n.act[i] *= 0.86
	}
	var next []pulse
	for _, p := range n.pending {
		if p.wait > 0 {
			p.wait--
			next = append(next, p)
			continue
		}
		if p.energy <= n.act[p.node] {
			continue
		}
		n.act[p.node] = p.energy
		if e := p.energy * 0.75; e > 0.2 {
			for _, j := range n.adj[p.node] {
				next = append(next, pulse{node: j, energy: e, wait: 2})
			}
		}
	}
	n.pending = next
}

var netShades = []lipgloss.Color{"#3b3355", "#5b3fa8", "#8b5cf6", "#c4b5fd", "#f5d0fe"}

var netStyles = func() []lipgloss.Style {
	out := make([]lipgloss.Style, len(netShades))
	for i, c := range netShades {
		out[i] = lipgloss.NewStyle().Foreground(c)
	}
	return out
}()

func shadeIndex(a float64) int {
	switch {
	case a > 0.8:
		return 4
	case a > 0.55:
		return 3
	case a > 0.3:
		return 2
	case a > 0.1:
		return 1
	}
	return 0
}

func (n *neuralNet) View() string {
	var b strings.Builder
	for r, row := range n.grid {
		var run strings.Builder
		runShade := -1
		flush := func() {
			if run.Len() > 0 {
				b.WriteString(netStyles[runShade].Render(run.String()))
				run.Reset()
			}
		}
		for c, ch := range row {
			level := 0.0
			if i, ok := n.nodeAt[[2]int{r, c}]; ok {
				level = n.act[i]
				if level < 0.12 {
					level = 0.12 // nodes always show faintly
				}
			} else if e, ok := n.edgeOf[[2]int{r, c}]; ok {
				level = (n.act[e[0]] + n.act[e[1]]) / 2 * 0.9
			}
			if sh := shadeIndex(level); sh != runShade {
				flush()
				runShade = sh
			}
			run.WriteRune(ch)
		}
		flush()
		if r < len(n.grid)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

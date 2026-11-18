package explorer

import "strings"

// DiffOp is the kind of a DiffLine.
type DiffOp int

// Diff line kinds.
const (
	// Context is a line present in both texts.
	Context DiffOp = iota
	// Add is a line only present in the new text.
	Add
	// Del is a line only present in the old text.
	Del
)

// DiffLine is one line of a line-based diff.
type DiffLine struct {
	Op   DiffOp
	Text string
}

// maxDiffCells bounds the LCS table; beyond it the diff degrades to
// "everything removed, then everything added".
const maxDiffCells = 250000

// LineDiff computes a line-by-line diff (longest common subsequence) between
// oldText and newText. Same algorithm as the web explorer's lineDiff.
func LineDiff(oldText, newText string) []DiffLine {
	a := strings.Split(oldText, "\n")
	b := strings.Split(newText, "\n")
	m, n := len(a), len(b)
	if m*n > maxDiffCells {
		out := make([]DiffLine, 0, m+n)
		for _, l := range a {
			out = append(out, DiffLine{Del, l})
		}
		for _, l := range b {
			out = append(out, DiffLine{Add, l})
		}
		return out
	}
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = 1 + dp[i+1][j+1]
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var out []DiffLine
	i, j := 0, 0
	for i < m && j < n {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{Context, a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, DiffLine{Del, a[i]})
			i++
		default:
			out = append(out, DiffLine{Add, b[j]})
			j++
		}
	}
	for ; i < m; i++ {
		out = append(out, DiffLine{Del, a[i]})
	}
	for ; j < n; j++ {
		out = append(out, DiffLine{Add, b[j]})
	}
	return out
}

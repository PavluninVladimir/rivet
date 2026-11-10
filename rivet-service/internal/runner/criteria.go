package runner

import (
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var criterionLine = regexp.MustCompile(`^CRITERION ([0-9]+): (OK|FAIL)(?:[ \t]+(.*))?$`)

// parseCriteria принимает маркеры строго с начала строки, последний на номер.
func parseCriteria(out string, count int) []*pb.CriterionResult {
	results := map[int]*pb.CriterionResult{}
	for _, line := range strings.Split(out, "\n") {
		m := criterionLine.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if m == nil {
			continue
		}
		index, err := strconv.Atoi(m[1])
		if err != nil || index < 1 || index > count {
			continue
		}
		c := &pb.CriterionResult{Index: int32(index), Ok: m[2] == "OK"}
		if !c.Ok {
			note := []rune(strings.TrimSpace(m[3]))
			c.Note = string(note[:min(1000, len(note))])
		}
		results[index] = c
	}
	indices := make([]int, 0, len(results))
	for i := range results {
		indices = append(indices, i)
	}
	slices.Sort(indices)
	var outResults []*pb.CriterionResult
	for _, i := range indices {
		outResults = append(outResults, results[i])
	}
	return outResults
}

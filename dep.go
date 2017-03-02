package main

type Dep struct {
	Name    string
	BuildID string
}

type DepSlice []Dep

func (s DepSlice) Less(i, j int) bool {
	return s[i].Name < s[j].Name
}
func (s DepSlice) Len() int      { return len(s) }
func (s DepSlice) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

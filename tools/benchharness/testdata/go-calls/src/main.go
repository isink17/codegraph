package main

import "fmt"

type Store struct{ items []string }

func (s *Store) Add(v string) { s.items = append(s.items, normalize(v)) }

func (s *Store) Len() int { return len(s.items) }

func normalize(v string) string { return trim(v) }

func trim(v string) string { return v }

func main() {
	s := &Store{}
	s.Add("a")
	fmt.Println(s.Len())
	report(s)
}

func report(s *Store) { fmt.Println(normalize("n"), s.Len()) }

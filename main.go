package main

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/julienschmidt/httprouter"
)

var (
	boards []Board
	tmpls  *template.Template
)

type MediumType uint8

const (
	MediumType_Image MediumType = iota
)

type Medium struct {
	Id        uint64
	Type      MediumType
	Filename  string
	Extension string
}

type Post struct {
	Id     uint64
	Posted time.Time
	Media  []Medium
	Text   string
}

type Thread struct {
	Post
	Posts []Post
}

func (t *Thread) LastPost() Post {
	l := len(t.Posts)
	if l == 0 {
		return t.Post
	}
	return t.Posts[l-1]
}

type Threads []Thread

func (ts Threads) Len() int {
	return len(ts)
}

func (ts Threads) Swap(i, j int) {
	ts[i], ts[j] = ts[j], ts[i]
}

func (ts Threads) Less(i, j int) bool {
	return ts[i].LastPost().Posted.After(ts[j].LastPost().Posted)
}

type Board struct {
	Name           string
	Threads        Threads
	Pages          uint
	ThreadsPerPage uint
	Write          sync.Mutex
	BoardTempl     string
	ThreadTempl    string
}

func (b *Board) AddThread(thread Thread) {
	b.Write.Lock()
	fmt.Printf("%+v\n", b.Threads)
	sort.Sort(b.Threads)
	if len(b.Threads) < cap(b.Threads) {
		b.Threads = append(b.Threads, thread)
	} else {
		for i := len(b.Threads) - 1; i > 0; i-- {
			b.Threads[i] = b.Threads[i-1]
		}
		b.Threads[0] = thread
	}
	b.Write.Unlock()
}

func (b *Board) GetThreadsOfPage(page uint) []Thread {
	if page > b.Pages {
		page = 0
	}
	max := b.AvailablePages()
	if page > max {
		page = max
	}
	o := page * b.ThreadsPerPage
	oe := o + b.ThreadsPerPage
	if oe > uint(len(b.Threads)) {
		oe = uint(len(b.Threads))
	}
	return b.Threads[o:oe]
}

func (b *Board) AvailablePages() uint {
	max := uint(len(b.Threads)) / b.ThreadsPerPage
	if (uint(len(b.Threads)) % b.ThreadsPerPage) != 0 {
		max++
	}
	return max
}

func (b *Board) GetThreadById(id uint64) (Thread, error) {
	for _, t := range b.Threads {
		if t.Id == id {
			return t, nil
		}
	}
	return Thread{}, errors.New("thread not found")
}

func indexHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {

}

func makeThreadHandler(board Board) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		id, err := strconv.ParseUint(ps.ByName("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		thread, err := board.GetThreadById(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		lastpage := board.AvailablePages()
		pages := make([]uint, lastpage)
		var i uint
		for i = 0; i < lastpage; i++ {
			pages[i] = i
		}
		tmpls.ExecuteTemplate(w, board.ThreadTempl, struct {
			Name   string
			Thread Thread
			Pages  []uint
		}{
			board.Name,
			thread,
			pages,
		})
	}
}

func makeBoardHandler(board Board, page uint) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		threads := board.GetThreadsOfPage(page)
		lastpage := board.AvailablePages()
		pages := make([]uint, lastpage)
		var i uint
		for i = 0; i < lastpage; i++ {
			pages[i] = i
		}
		tmpls.ExecuteTemplate(w, board.BoardTempl, struct {
			Name    string
			Threads []Thread
			Pages   []uint
		}{
			board.Name,
			threads,
			pages,
		})
	}
}

func main() {
	boards := []Board{
		{
			Name:           "b",
			Pages:          2,
			ThreadsPerPage: 10,
			Threads:        make([]Thread, 0, 2*10),
			BoardTempl:     "board.tmpl",
			ThreadTempl:    "thread.tmpl",
		},
		{
			Name:           "int",
			Pages:          10,
			ThreadsPerPage: 10,
			Threads:        make([]Thread, 0, 10*10),
			BoardTempl:     "board.tmpl",
			ThreadTempl:    "thread.tmpl",
		},
	}
	{
		for i := 0; i < 25; i++ {
			boards[0].AddThread(Thread{
				Post: Post{
					Media: []Medium{
						{
							uint64(i),
							MediumType_Image,
							"blorb.png",
							".png",
						},
						{
							uint64(i*2 + 1),
							MediumType_Image,
							"blorb.png",
							".png",
						},
					},
					Id:     uint64(i),
					Posted: time.Date(4000, 4, 5, 5, i, 42, 54, time.UTC),
					Text:   fmt.Sprint("hello world ", i),
				},
			})
		}
	}

	fmt.Printf("%#v\n", boards)
	{
		funcs := template.FuncMap{
			"loop": func(n int) []int {
				ints := make([]int, n)
				for i := 0; i < n; i++ {
					ints[i] = i
				}
				return ints
			},
		}
		t, err := template.New("_").Funcs(funcs).ParseGlob("./tmpl/*.tmpl")
		if err != nil {
			panic(err)
		}
		tmpls = t
	}

	router := httprouter.New()
	router.GET("/", indexHandler)
	for _, board := range boards {
		router.GET("/"+board.Name+"/", makeBoardHandler(board, 0))
		router.GET(fmt.Sprintf("/%s/thread/:id", board.Name), makeThreadHandler(board))
		var i uint
		for i = 0; i < board.Pages; i++ {
			router.GET(fmt.Sprintf("/%s/%d", board.Name, i), makeBoardHandler(board, i))
		}
	}
	router.ServeFiles("/media/*filepath", http.Dir("./media/"))
	log.Fatal(http.ListenAndServe(":8080", router))
}

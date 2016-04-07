package main

import (
	"errors"
	"fmt"
	"html/template"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/nfnt/resize"
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

func (t *Thread) AddPost(p Post) {
	t.Posts = append(t.Posts, p)
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

type BoardConfiguration struct {
	Name            string
	Pages           uint
	ThreadsPerPage  uint
	BoardTmpl       string
	ThreadTmpl      string
	MaxMediaPerPost uint
	MaxPostLength   uint
	MaxThumbWidth   uint
	MaxThumbHeight  uint
}

type Board struct {
	BoardConfiguration

	Threads      Threads
	Write        sync.Mutex
	PostCounter  uint64
	MediaCounter uint64
}

func (b *Board) AddThread(thread Thread) {
	sort.Sort(b.Threads)
	if len(b.Threads) < cap(b.Threads) {
		b.Threads = append(b.Threads, thread)
	} else {
		for i := len(b.Threads) - 1; i > 0; i-- {
			b.Threads[i] = b.Threads[i-1]
		}
		b.Threads[0] = thread
	}
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

func (b *Board) GetThreadById(id uint64) (*Thread, error) {
	for i, _ := range b.Threads {
		if b.Threads[i].Id == id {
			return &b.Threads[i], nil
		}
	}
	return nil, errors.New("thread not found")
}

func indexHandler(w http.ResponseWriter, r *http.Request, p httprouter.Params) {

}

func makeThreadHandler(board *Board) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		id, err := strconv.ParseUint(p.ByName("id"), 10, 64)
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
		maxmedia := make([]uint, board.MaxMediaPerPost)
		for i = 0; i < board.MaxMediaPerPost; i++ {
			maxmedia[i] = i
		}
		tmpls.ExecuteTemplate(w, board.ThreadTmpl, struct {
			Name     string
			Thread   Thread
			Pages    []uint
			MaxMedia []uint
		}{
			board.Name,
			*thread,
			pages,
			maxmedia,
		})
	}
}

func makeBoardHandler(board *Board, page uint) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		threads := board.GetThreadsOfPage(page)
		lastpage := board.AvailablePages()
		pages := make([]uint, lastpage)
		var i uint
		for i = 0; i < lastpage; i++ {
			pages[i] = i
		}
		maxmedia := make([]uint, board.MaxMediaPerPost)
		for i = 0; i < board.MaxMediaPerPost; i++ {
			maxmedia[i] = i
		}
		tmpls.ExecuteTemplate(w, board.BoardTmpl, struct {
			Name     string
			Threads  []Thread
			Pages    []uint
			MaxMedia []uint
		}{
			board.Name,
			threads,
			pages,
			maxmedia,
		})
	}
}

func processMediaOfRequest(board *Board, r *http.Request) ([]Medium, error) {
	err := r.ParseMultipartForm(1024 * 1024)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	m := r.MultipartForm
	var i uint
	files := make([]*multipart.FileHeader, 0, board.MaxMediaPerPost)
	for i = 0; i < board.MaxMediaPerPost; i++ {
		file := m.File[fmt.Sprint("file", i)]
		if len(file) == 0 {
			break
		}
		files = append(files, file[0])
	}
	media := make([]Medium, len(files))
	for i, file := range files {
		// TODO in case of error delete all the written files
		// TODO check if file is longer than limit
		var typ MediumType
		ext := strings.ToLower(filepath.Ext(file.Filename))
		if ext == ".gif" ||
			ext == ".jpg" ||
			ext == ".jpeg" ||
			ext == ".png" {
			typ = MediumType_Image
		} else {
			return nil, errors.New("invalid extension for file " + file.Filename)
		}
		medium := Medium{
			Id:        board.MediaCounter + uint64(i),
			Type:      typ,
			Filename:  file.Filename,
			Extension: ext,
		}
		switch typ {
		case MediumType_Image:
			{
				src, err := file.Open()
				if err != nil {
					return nil, err
				}
				defer src.Close()
				{
					img, format, err := image.Decode(src)
					if err != nil {
						return nil, err
					}
					thmb := resize.Thumbnail(
						board.MaxThumbWidth, board.MaxThumbWidth,
						img, resize.Bicubic)
					dstName := fmt.Sprint("./thumb/", board.Name,
						medium.Id, medium.Extension)
					dst, err := os.OpenFile(dstName, os.O_CREATE|os.O_WRONLY,
						0600)
					if err != nil {
						return nil, err
					}
					switch format {
					case "png":
						if err := png.Encode(dst, thmb); err != nil {
							dst.Close()
							return nil, err
						}
					case "jpeg":
						if err := jpeg.Encode(dst, thmb, nil); err != nil {
							dst.Close()
							return nil, err
						}
					case "gif":
						if err := gif.Encode(dst, thmb, nil); err != nil {
							dst.Close()
							return nil, err
						}
					}
					dst.Close()
				}
				src.Seek(0, 0)
				{
					dstName := fmt.Sprint("./media/", board.Name,
						medium.Id, medium.Extension)
					dst, err := os.OpenFile(dstName, os.O_CREATE|os.O_WRONLY,
						0600)
					if err != nil {
						return nil, err
					}
					if _, err := io.Copy(dst, src); err != nil {
						dst.Close()
						return nil, err
					}
					dst.Close()
				}
				media[i] = medium
			}
		}
	}
	return media, nil
}

func makePostHandler(board *Board) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		// TODO check IP for ban
		text := r.FormValue("text")
		if uint(len(text)) > board.MaxPostLength {
			http.Error(w, "text too long", http.StatusBadRequest)
			return
		}
		sthreadid := r.FormValue("thread")
		newthread := len(sthreadid) == 0
		if newthread {
			media, err := processMediaOfRequest(board, r)
			if err != nil {
				http.Error(w, "media upload failed", http.StatusBadRequest)
				return
			}
			board.Write.Lock()
			{
				thread := Thread{
					Post: Post{
						Id:     board.PostCounter,
						Posted: time.Now(),
						Media:  media,
						Text:   text,
					},
					Posts: []Post{},
				}
				board.PostCounter++
				board.MediaCounter += uint64(len(media))
				board.AddThread(thread)
			}
			board.Write.Unlock()
			http.Redirect(w, r, "/"+board.Name+"/", http.StatusFound)
			return
		}
		threadid, err := strconv.ParseUint(sthreadid, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		thread, err := board.GetThreadById(threadid)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		media, err := processMediaOfRequest(board, r)
		if err != nil {
			http.Error(w, "media upload failed", http.StatusBadRequest)
			return
		}
		board.Write.Lock()
		{
			post := Post{
				Id:     board.PostCounter,
				Posted: time.Now(),
				Media:  media,
				Text:   text,
			}
			board.PostCounter++
			board.MediaCounter += uint64(len(media))
			thread.AddPost(post)
		}
		board.Write.Unlock()
		http.Redirect(w, r,
			fmt.Sprintf("/%s/thread/%d", board.Name, thread.Id),
			http.StatusFound)
	}
}

func main() {
	// TODO load the board configuration from json
	boardconfigs := []BoardConfiguration{
		{
			Name:            "b",
			BoardTmpl:       "board.tmpl",
			ThreadTmpl:      "thread.tmpl",
			Pages:           2,
			ThreadsPerPage:  10,
			MaxMediaPerPost: 4,
			MaxPostLength:   200,
			MaxThumbWidth:   200,
			MaxThumbHeight:  200,
		},
		{
			Name:            "int",
			BoardTmpl:       "board.tmpl",
			ThreadTmpl:      "thread.tmpl",
			Pages:           2,
			ThreadsPerPage:  10,
			MaxMediaPerPost: 4,
			MaxPostLength:   200,
			MaxThumbWidth:   200,
			MaxThumbHeight:  200,
		},
	}

	boards := make([]Board, len(boardconfigs))
	for i, _ := range boards {
		boards[i].BoardConfiguration = boardconfigs[i]
		boards[i].Threads = make([]Thread, 0,
			boards[i].Pages*boards[i].ThreadsPerPage)
	}

	// TODO write routine that dumps a board in an interval to json
	// TODO load the json dumped data at start

	{ // test data, replace with code that loads from json
		board := &boards[0]
		for i := 0; i < 25; i++ {
			board.AddThread(Thread{
				Post: Post{
					Media: []Medium{
						{
							board.MediaCounter,
							MediumType_Image,
							"blorb.png",
							".png",
						},
						{
							board.MediaCounter + 1,
							MediumType_Image,
							"blorb.png",
							".png",
						},
					},
					Id:     board.PostCounter,
					Posted: time.Date(2000, 4, 5, 5, i, 42, 54, time.UTC),
					Text:   fmt.Sprint("hello world ", i),
				},
			})
			board.PostCounter++
			board.MediaCounter += 2
		}
	}

	{
		t, err := template.ParseGlob("./tmpl/*.tmpl")
		if err != nil {
			panic(err)
		}
		tmpls = t
	}

	router := httprouter.New()
	router.GET("/", indexHandler)
	for i, _ := range boards {
		board := &boards[i]
		router.GET("/"+board.Name+"/", makeBoardHandler(board, 0))
		router.POST("/"+board.Name+"/", makePostHandler(board))
		router.GET(fmt.Sprintf("/%s/thread/:id", board.Name),
			makeThreadHandler(board))
		var i uint
		for i = 0; i < board.Pages; i++ {
			router.GET(fmt.Sprintf("/%s/%d", board.Name, i),
				makeBoardHandler(board, i))
		}
	}
	router.ServeFiles("/thumb/*filepath", http.Dir("./thumb/"))
	router.ServeFiles("/media/*filepath", http.Dir("./media/"))
	log.Fatal(http.ListenAndServe(":8080", router))
}

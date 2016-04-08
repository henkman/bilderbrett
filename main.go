package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"image"
	"image/draw"
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
	logger *log.Logger
	boards []Board
	tmpls  *template.Template
)

type MediumType uint8

const (
	MediumType_Image MediumType = iota
	MediumType_Gif
	MediumType_Webm
	MediumType_Archive
	MediumType_Text
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
	Name                     string
	Pages                    uint
	ThreadsPerPage           uint
	BoardTmpl                string
	ThreadTmpl               string
	MaxMediaPerPost          uint
	MaxPostLength            uint
	MaxThumbWidth            uint
	MaxThumbHeight           uint
	MaxFileSize              uint64
	NewThreadsMustHaveMedium bool
	BackupInterval           time.Duration
}

type Board struct {
	BoardConfiguration `json:"-"`

	Threads      Threads
	Write        sync.Mutex `json:"-"`
	PostCounter  uint64
	MediaCounter uint64
}

func (b *Board) AddThread(thread Thread) {
	if len(b.Threads) < cap(b.Threads) {
		b.Threads = append(b.Threads, thread)
	} else {
		for i := len(b.Threads) - 1; i > 0; i-- {
			b.Threads[i] = b.Threads[i-1]
		}
		{ // NOTE delete media of dead thread
			for _, m := range b.Threads[0].Media {
				deleteMediumFiles(b, m)
			}
			for _, p := range b.Threads[0].Posts {
				for _, m := range p.Media {
					deleteMediumFiles(b, m)
				}
			}
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

func (b *Board) GetThreadById(id uint64) *Thread {
	for i, _ := range b.Threads {
		if b.Threads[i].Id == id {
			return &b.Threads[i]
		}
	}
	return nil
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
		thread := board.GetThreadById(id)
		if thread == nil {
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
		// TODO find a better way to get for loops in templates
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

func fitThumbnail(ow, oh, mw, mh uint) (uint, uint, bool) {
	if ow < mw && oh < mh {
		return ow, oh, false
	}
	nw, nh := ow, oh
	if nw > mw {
		nh = uint(oh * mw / ow)
		if nh < 1 {
			nh = 1
		}
		nw = mw
	}
	if nh > mh {
		nw = uint(nw * mh / nh)
		if nw < 1 {
			nw = 1
		}
		nh = mh
	}
	return nw, nh, true
}

func processMediaOfRequest(board *Board, r *http.Request) ([]Medium, error) {
	err := r.ParseMultipartForm(1024 * 1024)
	if err != nil {
		return nil, errors.New("internal error ParseMultipartForm()")
	}
	files := make([]*multipart.FileHeader, 0, board.MaxMediaPerPost)
	{
		var i uint
		for i = 0; i < board.MaxMediaPerPost; i++ {
			file := r.MultipartForm.File[fmt.Sprint("file", i)]
			if len(file) != 0 {
				files = append(files, file[0])
			}
		}
	}
	media := make([]Medium, 0, len(files))
	for i, file := range files {
		var medium Medium
		{
			var typ MediumType
			ext := strings.ToLower(filepath.Ext(file.Filename))
			if ext == ".jpg" ||
				ext == ".jpeg" ||
				ext == ".png" {
				typ = MediumType_Image
			} else if ext == ".gif" {
				typ = MediumType_Gif
			} else {
				return media, errors.New("invalid extension for file " +
					file.Filename)
			}
			medium = Medium{
				Id:        board.MediaCounter + uint64(i),
				Type:      typ,
				Filename:  file.Filename,
				Extension: ext,
			}
		}
		{
			src, err := file.Open()
			if err != nil {
				return media, errors.New("file.Open()")
			}
			defer src.Close()
			size, err := src.Seek(0, os.SEEK_END)
			if err != nil {
				return media, errors.New("src.Seek(END)")
			}
			if uint64(size) > board.MaxFileSize {
				return media, errors.New("file " + file.Filename + " too big")
			}
			src.Seek(0, os.SEEK_SET)

			// TODO speed up thumbnail creation
			// TODO do not write thumbnails smaller than max allowed
			switch medium.Type {
			case MediumType_Image:
				{
					img, format, err := image.Decode(src)
					if err != nil {
						return media, errors.New("image.Decode()")
					}
					b := img.Bounds()
					var thmb image.Image
					if w, h, ok := fitThumbnail(uint(b.Dx()), uint(b.Dy()),
						board.MaxThumbWidth, board.MaxThumbWidth); ok {
						thmb = resize.Resize(w, h, img,
							resize.NearestNeighbor)
					} else {
						thmb = img
					}
					name := fmt.Sprint("./thumb/", board.Name,
						medium.Id, medium.Extension)
					dst, err := os.OpenFile(name,
						os.O_CREATE|os.O_WRONLY, 0600)
					if err != nil {
						return media, errors.New("os.OpenFile(thumb)")
					}
					switch format {
					case "png":
						if err := png.Encode(dst, thmb); err != nil {
							dst.Close()
							return media, errors.New("png.Encode()")
						}
					case "jpeg":
						if err := jpeg.Encode(dst, thmb, nil); err != nil {
							dst.Close()
							return media, errors.New("jpeg.Encode()")
						}
					}
					dst.Close()
					src.Seek(0, os.SEEK_SET)
				}
			case MediumType_Gif:
				{
					g, err := gif.DecodeAll(src)
					if err != nil {
						return media, errors.New("gif.DecodeAll()")
					}
					if w, h, ok := fitThumbnail(
						uint(g.Config.Width), uint(g.Config.Height),
						board.MaxThumbWidth, board.MaxThumbHeight); ok {
						for i, _ := range g.Image {
							p := &g.Image[i]
							r := resize.Resize(w, h, *p, resize.NearestNeighbor)
							b := r.Bounds()
							(*p).Pix = (*p).Pix[:1*w*h]
							(*p).Stride = int(1 * w)
							(*p).Rect = b
							draw.Draw(*p, b, r, b.Min, draw.Src)
						}
						g.Config.Width = int(w)
						g.Config.Height = int(h)
					}
					name := fmt.Sprint("./thumb/", board.Name,
						medium.Id, medium.Extension)
					dst, err := os.OpenFile(name,
						os.O_CREATE|os.O_WRONLY, 0600)
					if err != nil {
						return media, errors.New("os.OpenFile(thumb)")
					}
					if err := gif.EncodeAll(dst, g); err != nil {
						dst.Close()
						return media, errors.New("gif.EncodeAll()")
					}
					dst.Close()
					src.Seek(0, os.SEEK_SET)
				}
			case MediumType_Webm:
				{
				}
			case MediumType_Archive:
				{
				}
			case MediumType_Text:
				{
				}
			}
			{ // NOTE write original file
				name := fmt.Sprint("./media/", board.Name,
					medium.Id, medium.Extension)
				dst, err := os.OpenFile(name,
					os.O_CREATE|os.O_WRONLY, 0600)
				if err != nil {
					return media, errors.New("os.OpenFile(original)")
				}
				if _, err := io.Copy(dst, src); err != nil {
					dst.Close()
					return media, errors.New("io.Copy()")
				}
				dst.Close()
			}
			media = append(media, medium)
		}
	}
	return media, nil
}

func deleteMediumFiles(b *Board, m Medium) {
	o := fmt.Sprint("./media/", b.Name, m.Id, m.Extension)
	if _, err := os.Stat(o); err == nil {
		os.Remove(o)
	}
	switch m.Type {
	case MediumType_Image:
		{
			t := fmt.Sprint("./thumb/", b.Name, m.Id, m.Extension)
			if _, err := os.Stat(t); err == nil {
				os.Remove(t)
			}
		}
	case MediumType_Webm:
		{
		}
	case MediumType_Archive:
		{
		}
	case MediumType_Text:
		{
		}
	}
}

func makePostHandler(b *Board) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		// TODO add captcha (solve captcha once then add to session for a time)
		// TODO check IP for ban
		// TODO rate limit posting
		text := r.FormValue("text")
		if uint(len(text)) > b.MaxPostLength {
			http.Error(w, "text too long", http.StatusBadRequest)
			return
		}
		sthreadid := r.FormValue("thread")
		newthread := len(sthreadid) == 0
		if newthread {
			b.Write.Lock()
			{
				// TODO find a way to process media without write lock
				media, err := processMediaOfRequest(b, r)
				if err != nil {
					for _, m := range media {
						deleteMediumFiles(b, m)
					}
					b.Write.Unlock()
					http.Error(w, "media upload failed: "+err.Error(),
						http.StatusBadRequest)
					return
				}
				if b.NewThreadsMustHaveMedium && len(media) == 0 {
					http.Error(w, "threads must include a medium",
						http.StatusBadRequest)
					return
				}
				thread := Thread{
					Post: Post{
						Id:     b.PostCounter,
						Posted: time.Now().UTC(),
						Media:  media,
						Text:   text,
					},
					Posts: []Post{},
				}
				b.PostCounter++
				b.MediaCounter += uint64(len(media))
				b.AddThread(thread)
				sort.Sort(b.Threads)
			}
			b.Write.Unlock()
			http.Redirect(w, r, "/"+b.Name+"/", http.StatusFound)
		} else {
			threadid, err := strconv.ParseUint(sthreadid, 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			thread := b.GetThreadById(threadid)
			if thread == nil {
				http.NotFound(w, r)
				return
			}
			// NOTE sort.Sort invalidates the thread pointer so we save id here
			tid := thread.Id
			b.Write.Lock()
			{
				// TODO find a way to process media without write lock
				media, err := processMediaOfRequest(b, r)
				if err != nil {
					for _, m := range media {
						deleteMediumFiles(b, m)
					}
					b.Write.Unlock()
					http.Error(w, "media upload failed: "+err.Error(),
						http.StatusBadRequest)
					return
				}
				post := Post{
					Id:     b.PostCounter,
					Posted: time.Now().UTC(),
					Media:  media,
					Text:   text,
				}
				b.PostCounter++
				b.MediaCounter += uint64(len(media))
				thread.AddPost(post)
				sort.Sort(b.Threads)
			}
			b.Write.Unlock()
			http.Redirect(w, r,
				fmt.Sprintf("/%s/thread/%d", b.Name, tid),
				http.StatusFound)
		}
	}
}

func backupRoutine(b *Board) {
	tick := time.NewTicker(b.BackupInterval)
	for {
		<-tick.C
		logger.Println("starting backup of", b.Name)
		b.Write.Lock()
		{
			fd, err := os.OpenFile(
				fmt.Sprint("./backup/", b.Name, ".json"),
				os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
			if err != nil {
				logger.Println("backup of", b.Name, "failed:",
					err.Error())
				b.Write.Unlock()
				continue
			}
			if err := json.NewEncoder(fd).Encode(b); err != nil {
				logger.Println("backup of", b.Name, "failed:",
					err.Error())
				fd.Close()
				b.Write.Unlock()
				continue
			}
			fd.Close()
		}
		b.Write.Unlock()
		logger.Println("backup of", b.Name, "successful")
	}
}

func main() {
	// TODO also/only write log to file
	logger = log.New(os.Stderr, "", log.LUTC)

	var boardconfigs []BoardConfiguration
	{
		fd, err := os.OpenFile(
			fmt.Sprint("./config.json"),
			os.O_RDONLY, 0600)
		if err != nil {
			logger.Fatal("could not read config.json")
			return
		}
		jd := json.NewDecoder(fd)
		var config struct {
			Boards []BoardConfiguration
		}
		if err := jd.Decode(&config); err != nil {
			logger.Fatal("could not parse config.json: ", err)
			fd.Close()
			return
		}
		fd.Close()
		if len(config.Boards) == 0 {
			logger.Fatal("no boards configured in config.json")
			return
		}
		boardconfigs = config.Boards
	}
	{
		t, err := template.ParseGlob("./tmpl/*.tmpl")
		if err != nil {
			logger.Fatal("could not load templates: ", err)
			return
		}
		tmpls = t
	}
	boards := make([]Board, len(boardconfigs))
	for i, _ := range boards {
		b := &boards[i]
		b.BoardConfiguration = boardconfigs[i]
		b.Threads = make([]Thread, 0, b.Pages*b.ThreadsPerPage)
		{
			fd, err := os.OpenFile(
				fmt.Sprint("./backup/", b.Name, ".json"),
				os.O_RDONLY, 0600)
			if err == nil {
				jd := json.NewDecoder(fd)
				var bu Board
				jd.Decode(&bu)
				fd.Close()
				b.MediaCounter = bu.MediaCounter
				b.PostCounter = bu.PostCounter
				b.Threads = append(b.Threads, bu.Threads...)
			}
		}
		if b.BackupInterval > 0 {
			go backupRoutine(b)
		}
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
	logger.Fatal(http.ListenAndServe(":8080", router))
}

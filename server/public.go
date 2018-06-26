package server

import (
	"blockbook/api"
	"blockbook/bchain"
	"blockbook/common"
	"blockbook/db"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
)

const blockbookAbout = "Blockbook - blockchain indexer for TREZOR wallet https://trezor.io/. Do not use for any other purpose."

// PublicServer is handle to public http server
type PublicServer struct {
	binding     string
	certFiles   string
	socketio    *SocketIoServer
	https       *http.Server
	db          *db.RocksDB
	txCache     *db.TxCache
	chain       bchain.BlockChain
	chainParser bchain.BlockChainParser
	api         *api.Worker
	explorerURL string
	metrics     *common.Metrics
	is          *common.InternalState
	txTpl       *template.Template
}

// NewPublicServerS creates new public server http interface to blockbook and returns its handle
func NewPublicServer(binding string, certFiles string, db *db.RocksDB, chain bchain.BlockChain, txCache *db.TxCache, explorerURL string, metrics *common.Metrics, is *common.InternalState) (*PublicServer, error) {

	api, err := api.NewWorker(db, chain, txCache)
	if err != nil {
		return nil, err
	}

	socketio, err := NewSocketIoServer(db, chain, txCache, metrics, is)
	if err != nil {
		return nil, err
	}

	addr, path := splitBinding(binding)
	serveMux := http.NewServeMux()
	https := &http.Server{
		Addr:    addr,
		Handler: serveMux,
	}

	s := &PublicServer{
		binding:     binding,
		certFiles:   certFiles,
		https:       https,
		api:         api,
		socketio:    socketio,
		db:          db,
		txCache:     txCache,
		chain:       chain,
		chainParser: chain.GetChainParser(),
		explorerURL: explorerURL,
		metrics:     metrics,
		is:          is,
	}

	// favicon
	serveMux.Handle(path+"favicon.ico", http.FileServer(http.Dir("./static/")))
	// support for tests of socket.io interface
	serveMux.Handle(path+"test.html", http.FileServer(http.Dir("./static/")))
	// redirect to Bitcore for details of transaction
	serveMux.HandleFunc(path+"tx/", s.txRedirect)
	serveMux.HandleFunc(path+"address/", s.addressRedirect)
	// explorer
	serveMux.HandleFunc(path+"explorer/tx/", s.explorerTx)
	// API calls
	serveMux.HandleFunc(path+"api/block-index/", s.apiBlockIndex)
	serveMux.HandleFunc(path+"api/tx/", s.apiTx)
	// handle socket.io
	serveMux.Handle(path+"socket.io/", socketio.GetHandler())
	// default handler
	serveMux.HandleFunc(path, s.index)

	s.txTpl = template.Must(template.New("tx").ParseFiles("./static/templates/tx.html", "./static/templates/base.html"))

	return s, nil
}

// Run starts the server
func (s *PublicServer) Run() error {
	if s.certFiles == "" {
		glog.Info("public server: starting to listen on http://", s.https.Addr)
		return s.https.ListenAndServe()
	}
	glog.Info("public server starting to listen on https://", s.https.Addr)
	return s.https.ListenAndServeTLS(fmt.Sprint(s.certFiles, ".crt"), fmt.Sprint(s.certFiles, ".key"))
}

// Close closes the server
func (s *PublicServer) Close() error {
	glog.Infof("public server: closing")
	return s.https.Close()
}

// Shutdown shuts down the server
func (s *PublicServer) Shutdown(ctx context.Context) error {
	glog.Infof("public server: shutdown")
	return s.https.Shutdown(ctx)
}

// OnNewBlockHash notifies users subscribed to bitcoind/hashblock about new block
func (s *PublicServer) OnNewBlockHash(hash string) {
	s.socketio.OnNewBlockHash(hash)
}

// OnNewTxAddr notifies users subscribed to bitcoind/addresstxid about new block
func (s *PublicServer) OnNewTxAddr(txid string, addr string) {
	s.socketio.OnNewTxAddr(txid, addr)
}

func splitBinding(binding string) (addr string, path string) {
	i := strings.Index(binding, "/")
	if i >= 0 {
		return binding[0:i], binding[i:]
	}
	return binding, "/"
}

func joinURL(base string, part string) string {
	if len(base) > 0 {
		if len(base) > 0 && base[len(base)-1] == '/' && len(part) > 0 && part[0] == '/' {
			return base + part[1:]
		}
		return base + part
	}
	return part
}

func (s *PublicServer) txRedirect(w http.ResponseWriter, r *http.Request) {
	if s.explorerURL != "" {
		http.Redirect(w, r, joinURL(s.explorerURL, r.URL.Path), 302)
		s.metrics.ExplorerViews.With(common.Labels{"action": "tx"}).Inc()
	}
}

func (s *PublicServer) addressRedirect(w http.ResponseWriter, r *http.Request) {
	if s.explorerURL != "" {
		http.Redirect(w, r, joinURL(s.explorerURL, r.URL.Path), 302)
		s.metrics.ExplorerViews.With(common.Labels{"action": "address"}).Inc()
	}
}

func (s *PublicServer) explorerTx(w http.ResponseWriter, r *http.Request) {
	var tx *api.Tx
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		txid := r.URL.Path[i+1:]
		bestheight, _, err := s.db.GetBestBlock()
		if err == nil {
			tx, err = s.api.GetTransaction(txid, bestheight, true)
		} else {
			glog.Error(err)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		CoinName string
		Tx       *api.Tx
	}{s.is.Coin, tx}
	if err := s.txTpl.ExecuteTemplate(w, "base.html", data); err != nil {
		glog.Error(err)
	}
}

type resAboutBlockbookPublic struct {
	Coin            string    `json:"coin"`
	Host            string    `json:"host"`
	Version         string    `json:"version"`
	GitCommit       string    `json:"gitcommit"`
	BuildTime       string    `json:"buildtime"`
	InSync          bool      `json:"inSync"`
	BestHeight      uint32    `json:"bestHeight"`
	LastBlockTime   time.Time `json:"lastBlockTime"`
	InSyncMempool   bool      `json:"inSyncMempool"`
	LastMempoolTime time.Time `json:"lastMempoolTime"`
	About           string    `json:"about"`
}

func (s *PublicServer) index(w http.ResponseWriter, r *http.Request) {
	vi := common.GetVersionInfo()
	ss, bh, st := s.is.GetSyncState()
	ms, mt, _ := s.is.GetMempoolSyncState()
	a := resAboutBlockbookPublic{
		Coin:            s.is.Coin,
		Host:            s.is.Host,
		Version:         vi.Version,
		GitCommit:       vi.GitCommit,
		BuildTime:       vi.BuildTime,
		InSync:          ss,
		BestHeight:      bh,
		LastBlockTime:   st,
		InSyncMempool:   ms,
		LastMempoolTime: mt,
		About:           blockbookAbout,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	buf, err := json.MarshalIndent(a, "", "    ")
	if err != nil {
		glog.Error(err)
	}
	w.Write(buf)
}

func (s *PublicServer) apiBlockIndex(w http.ResponseWriter, r *http.Request) {
	type resBlockIndex struct {
		BlockHash string `json:"blockHash"`
		About     string `json:"about"`
	}
	var err error
	var hash string
	height := -1
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		if h, err := strconv.Atoi(r.URL.Path[i+1:]); err == nil {
			height = h
		}
	}
	if height >= 0 {
		hash, err = s.db.GetBlockHash(uint32(height))
	} else {
		_, hash, err = s.db.GetBestBlock()
	}
	if err != nil {
		glog.Error(err)
	} else {
		r := resBlockIndex{
			BlockHash: hash,
			About:     blockbookAbout,
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(r)
	}
}

func (s *PublicServer) apiTx(w http.ResponseWriter, r *http.Request) {
	var tx *api.Tx
	var err error
	if i := strings.LastIndexByte(r.URL.Path, '/'); i > 0 {
		txid := r.URL.Path[i+1:]
		bestheight, _, err := s.db.GetBestBlock()
		if err == nil {
			tx, err = s.api.GetTransaction(txid, bestheight, true)
		} else {
			glog.Error(err)
		}
	}
	if err == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(tx)
	}
}

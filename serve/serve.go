package serve

import (
    "compress/gzip"
    "encoding/base64"
    "fmt"
    "github.com/elazarl/goproxy"
    "github.com/elazarl/goproxy/ext/auth"
    "github.com/googollee/go-socket.io"
    "github.com/hidu/goutils"
    "github.com/robertkrimen/otto"
    "io/ioutil"
    "log"
    "math/rand"
    "net"
    "net/http"
    "net/http/httputil"
    "net/url"
    "os"
    "reflect"
    "strconv"
    "strings"
    "sync"
    "time"
)

var js *otto.Otto

type ProxyServe struct {
    Port    int
    Goproxy *goproxy.ProxyHttpServer

    AuthType int

    mydb      *TieDb
    ws        *socketio.SocketIOServer
    wsClients map[string]*wsClient
    startTime time.Time

    MaxResSaveLength int64
    RewriteJs        string
    RewriteJsPath    string
    RewriteJsFn      otto.Value
    mu               sync.RWMutex

    Users map[string]string
    Debug bool
}

type wsClient struct {
    ns               *socketio.NameSpace
    user             string
    filter_client_ip string
    filter_hide      []string
    filter_url       []string
}

type kvType map[string]interface{}

func (ser *ProxyServe) ServeHTTP(w http.ResponseWriter, req *http.Request) {
    host, port, _ := net.SplitHostPort(req.Host)
    port_int, _ := strconv.Atoi(port)
    isLocalReq := port_int == ser.Port
    if isLocalReq {
        isLocalReq = IsLocalIp(host)
    }

    if isLocalReq {
        ser.handleLocalReq(w, req)
    } else {
        ser.Goproxy.ServeHTTP(w, req)
    }
}

func (ser *ProxyServe) Start() {
    ser.Goproxy = goproxy.NewProxyHttpServer()
    ser.Goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
        if ser.Debug {
            req_dump_debug, _ := httputil.DumpRequest(req, false)
            log.Println("DEBUG req BEFORE:\n", string(req_dump_debug))
        }
        authInfo := getAuthorInfo(req)
        uname := "guest"
        //fmt.Println("authInfo",authInfo)
        if authInfo != nil {
            uname = authInfo.Name
        }
        //fmt.Println("uname",uname)
        for k, _ := range req.Header {
            if len(k) > 5 && k[:6] == "Proxy-" {
                req.Header.Del(k)
            }
        }
        if ser.AuthType > 0 && ((ser.AuthType == 2 && authInfo == nil) || (ser.AuthType == 1 && !ser.CheckUserLogin(authInfo))) {
            log.Println("login required", req.RemoteAddr, authInfo)
            return nil, auth.BasicUnauthorized(req, "pproxy auth need")
        }

        ser.reqRewrite(req)

        if ser.Debug {
            req_dump_debug, _ := httputil.DumpRequest(req, false)
            log.Println("DEBUG req AFTER:\n", string(req_dump_debug))
        }

        logdata := kvType{}
        logdata["host"] = req.Host
        logdata["header"] = map[string][]string(req.Header)
        logdata["url"] = req.URL.String()
        logdata["path"] = req.URL.Path
        logdata["cookies"] = req.Cookies()
        logdata["now"] = time.Now().Unix()
        logdata["session_id"] = ctx.Session
        logdata["user"] = uname
        logdata["client_ip"] = req.RemoteAddr
        logdata["form_get"] = req.URL.Query()

        if strings.Contains(req.Header.Get("Content-Type"), "x-www-form-urlencoded") {
            buf := forgetRead(&req.Body)
            var body_str string
            content_enc := req.Header.Get("Content-Encoding")
            if content_enc == "gzip" {
                gr, gzip_err := gzip.NewReader(buf)
                defer gr.Close()
                if gzip_err == nil {
                    bd_bt, _ := ioutil.ReadAll(gr)
                    body_str = string(bd_bt)
                } else {
                    log.Println("unzip body failed", gzip_err)
                }
            } else {
                body_str = buf.String()
            }
            post_vs, post_e := url.ParseQuery(body_str)
            if post_e != nil {
                log.Println("parse post err", post_e)
            }
            logdata["form_post"] = post_vs
        }

        req_dump, err_dump := httputil.DumpRequest(req, true)
        if err_dump != nil {
            log.Println("dump request failed")
            req_dump = []byte("dump failed")
        }
        logdata["dump"] = base64.StdEncoding.EncodeToString(req_dump)
        req_uid := NextUid() + uint64(ctx.Session)

        ctx.UserData = req_uid

        rewrite := make(map[string]string)
        url_new := req.URL.String()

        if url_new != logdata["url"] {
            rewrite["url"] = url_new
        }

        logdata["rewrite"] = rewrite

        err := ser.mydb.RequestTable.InsertRecovery(req_uid, logdata)
        log.Println("save_req", ctx.Session, req.URL.String(), "req_docid=", req_uid, err, rewrite)

        if err != nil {
            log.Println(err)
            return req, nil
        }

        ser.Broadcast_Req(req, ctx.Session, req_uid, uname)

        return req, nil
    })

    ser.Goproxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
        if resp != nil {
            resp.Header.Set("Connection", "close")
        }
        if resp == nil || resp.Request == nil {
            return resp
        }
        //		fmt.Println("resp.Header:",resp.Header)
        ser.logResponse(resp, ctx)
        return resp
    })

    addr := fmt.Sprintf("%s:%d", "", ser.Port)
    log.Println("proxy listen at ", addr)
    ser.initWs()
    err := http.ListenAndServe(addr, ser)
    log.Println(err)
}

/**
*log response if the req has log
 */
func (ser *ProxyServe) logResponse(res *http.Response, ctx *goproxy.ProxyCtx) {
    if ctx.UserData == nil || reflect.TypeOf(ctx.UserData).Kind() != reflect.Uint64 {
        log.Println("err,userdata not reqid,log res skip")
        return
    }
    req_uid := ctx.UserData.(uint64)
    data := kvType{}
    data["session_id"] = ctx.Session
    data["now"] = time.Now().Unix()
    data["header"] = map[string][]string(res.Header)
    data["status"] = res.StatusCode
    data["content_length"] = res.ContentLength

    res_dump, dump_err := httputil.DumpResponse(res, false)
    if dump_err != nil {
        log.Println("dump res err", dump_err)
        res_dump = []byte("dump res failed")
    }
    data["dump"] = base64.StdEncoding.EncodeToString(res_dump)
    //   data["cookies"]=res.Cookies()

    body := []byte("pproxy skip")
    if res.ContentLength <= ser.MaxResSaveLength {
        buf := forgetRead(&res.Body)
        body = buf.Bytes()
    }
    data["body"] = base64.StdEncoding.EncodeToString(body)

    err := ser.mydb.ResponseTable.InsertRecovery(req_uid, data)
    log.Println("save_res [", req_uid, "]", err)
    if err != nil {
        log.Println(err)
        return
    }
}

func (ser *ProxyServe) GetResponseByDocid(docid uint64) (res_data kvType) {
    id, err := ser.mydb.ResponseTable.Read(docid, &res_data)
    if err != nil {
        log.Println("read res by docid failed,docid=", docid, "id=", id, err)
    }
    //  fmt.Println(docid,res_data)
    return res_data
}
func (ser *ProxyServe) GetRequestByDocid(docid uint64) (req_data kvType) {
    id, err := ser.mydb.RequestTable.Read(docid, &req_data)
    if err != nil {
        log.Println("read req by docid failed,docid=", docid, "id=", id, err)
    }
    return req_data
}

func NewProxyServe(data_dir string, jsPath string, port int) *ProxyServe {
    proxy := new(ProxyServe)
    proxy.Port = port
    js = otto.New()

    if goutils.File_exists(jsPath) {
        proxy.RewriteJsPath = jsPath
        script, err := ioutil.ReadFile(jsPath)
        if err == nil {
            err = proxy.parseAndSaveRewriteJs(string(script))
            if err != nil {
                fmt.Println("load rewrite js failed:", err)
                os.Exit(-1)
            }
        }
    }

    proxy.mydb = NewTieDb(fmt.Sprintf("%s/%d/", data_dir, port))
    proxy.startTime = time.Now()
    proxy.MaxResSaveLength = 2 * 1024 * 1024

    rand.Seed(time.Now().UnixNano())
    //   proxy.mydb.StartGcTimer(60,store_time)
    return proxy
}

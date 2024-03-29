package rest_client

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
)

// RestClientError  错误信息
type RestClientError struct {
	Msg  string
	Code string
}

func (err *RestClientError) Error() string {
	return err.Msg
}

// NewRestClientError  错误创建
func NewRestClientError(code string, msg string) *RestClientError {
	return &RestClientError{
		Code: code,
		Msg:  msg,
	}
}

// RestEvent  事件接口,用于暴露对外请求的时的信息
type RestEvent interface {
	RequestStart(method, url string)                         //开始请求时回调
	RequestRead(p []byte)                                    //成功时读取请求数据回调
	ResponseHeader(HttpCode int, header map[string][]string) //成功返回HEADER时回调
	ResponseRead(p []byte)                                   //成功时读取请求内容
	ResponseFinish(err error)                                //内容读取完时回调,不存在错误时err为nil
	ResponseCheck(err error)                                 //检测返回内容是否正常,正常时err为nil
}

// RestEventNoop  默认事件处理
type RestEventNoop struct{}

func (event *RestEventNoop) RequestStart(_, _ string)                    {}
func (event *RestEventNoop) RequestRead(_ []byte)                        {}
func (event *RestEventNoop) ResponseHeader(_ int, _ map[string][]string) {}
func (event *RestEventNoop) ResponseRead(_ []byte)                       {}
func (event *RestEventNoop) ResponseFinish(_ error)                      {}
func (event *RestEventNoop) ResponseCheck(_ error)                       {}

func NewRestEventNoop() *RestEventNoop {
	return &RestEventNoop{}
}

//RestRequestReader 对请求io.Reader封装,用于读取内容时事件回调
type RestRequestReader struct {
	reader io.Reader
	event  RestEvent
}

func NewRestRequestReader(reader io.Reader, event RestEvent) *RestRequestReader {
	return &RestRequestReader{
		reader: reader,
		event:  event,
	}
}
func (read *RestRequestReader) Read(p []byte) (int, error) {
	if read.reader == nil {
		return 0, NewRestClientError("10", "request reader is empty")
	}
	n, err := read.reader.Read(p)
	if read.event != nil && n > 0 {
		read.event.RequestRead(p[0:n])
	}
	return n, err
}

// RestBuild 执行请求
type RestBuild interface {
	BuildRequest(ctx context.Context, config *RestClient, key int, param interface{}, callerInfo *RestCallerInfo) *RestResult
}

type RestJsonResult interface {
	CheckJsonResult(res string) error
}

// RestConfig 执行请求
type RestConfig interface {
	GetName() string
}

// RestApi 接口定义
type RestApi interface {
	ConfigBuilds(ctx context.Context) (map[int]RestBuild, error)
	ConfigName(ctx context.Context) (string, error)
}

// RestTokenApi 带TOKEN的接口定义
type RestTokenApi interface {
	RestApi
	Token(ctx context.Context) (string, error)
}

//RestClient 请求
type RestClient struct {
	Api       RestApi
	config    map[string]RestConfig
	transport *http.Transport
}

//GetTransport 公共的Transport
func (client *RestClient) GetTransport() *http.Transport {
	return client.transport
}

//GetConfig 获取当前使用配置
func (client *RestClient) GetConfig(ctx context.Context) (RestConfig, error) {
	configName, err := client.Api.ConfigName(ctx)
	if err != nil {
		return nil, err
	}
	config, ok := client.config[configName]
	if !ok {
		return nil, NewRestClientError("1", "rest config is exits:"+configName)
	}
	return config, nil
}

//Do 执行请求
func (client *RestClient) Do(ctx context.Context, key int, param interface{}) chan *RestResult {
	rc := make(chan *RestResult, 1)
	reqs, err := client.Api.ConfigBuilds(ctx)
	if err != nil {
		rc <- NewRestResultFromError(err, nil)
		return rc
	}
	build, find := reqs[key]
	if !find {
		rc <- NewRestResultFromError(NewRestClientError("2", "not find rest api"), nil)
		close(rc)
	} else {
		caller := callerFileInfo("rest_client/rest_client.go", 1, 15)
		go func() {
			defer func() {
				if info := recover(); info != nil {
					rc <- NewRestResultFromError(NewRestClientError("3", fmt.Sprintf("panic %v", info)), nil)
					close(rc)
				}
			}()
			res := build.BuildRequest(ctx, client, key, param, caller)
			rc <- res
			close(rc)
		}()
	}
	return rc
}

//RestResult 请求接口后返回数据结构
type RestResult struct {
	event          RestEvent
	build          RestBuild
	response       *http.Response
	body           string
	bodyReadOffset int
	err            error
}

//NewRestResultFromError 创建一个错误的请求结果
func NewRestResultFromError(err error, event RestEvent) *RestResult {
	result := &RestResult{
		event:          event,
		build:          nil,
		bodyReadOffset: -1,
		body:           "",
		err:            err,
		response:       nil,
	}
	if event != nil {
		event.ResponseFinish(err)
	}
	return result
}

//NewRestResult 创建一个正常请求结果
//@param event 可以为nil
func NewRestResult(build RestBuild, response *http.Response, event RestEvent) *RestResult {
	result := &RestResult{
		event:          event,
		build:          build,
		bodyReadOffset: -1,
		body:           "",
		err:            nil,
		response:       response,
	}
	if event != nil && response != nil {
		event.ResponseHeader(response.StatusCode, response.Header)
	}
	return result
}

//NewRestBodyResult 创建外部已经读取Response BODY的请求结果
//@param response 可以为nil
//@param event 可以为nil
func NewRestBodyResult(build RestBuild, body string, response *http.Response, event RestEvent) *RestResult {
	result := &RestResult{
		event:          event,
		build:          build,
		bodyReadOffset: 0,
		body:           body,
		err:            nil,
		response:       response,
	}
	if event != nil {
		if response != nil {
			event.ResponseHeader(response.StatusCode, response.Header)
		}
		event.ResponseFinish(nil)
	}
	return result
}

//Header 获取返回HEADER
func (res *RestResult) Header() (error, *http.Header) {
	if res.err != nil {
		return res.err, nil
	}
	if res.response == nil || res.response.Header == nil {
		return nil, &http.Header{}
	}
	return nil, &res.response.Header
}

//Read 读取接口
func (res *RestResult) Read(p []byte) (int, error) {
	if res.err != nil {
		return 0, res.err
	}
	if res.bodyReadOffset >= 0 {
		bDat := []byte(res.body)
		pLen := len(p)
		sLen := len(bDat[res.bodyReadOffset:])
		if sLen == 0 {
			return 0, io.EOF
		}
		if sLen > pLen {
			tmp := bDat[res.bodyReadOffset : res.bodyReadOffset+pLen]
			copy(p[0:pLen], tmp)
			res.bodyReadOffset += pLen
			return pLen, nil
		} else {
			tmp := bDat[res.bodyReadOffset : res.bodyReadOffset+sLen]
			copy(p[0:sLen], tmp)
			res.bodyReadOffset += sLen
			return sLen, io.EOF
		}
	} else {
		if res.response == nil {
			return 0, io.EOF
		}
		n, err := res.response.Body.Read(p)
		if n > 0 {
			res.event.ResponseRead(p[0:n])
		}
		if err == io.EOF {
			if res.event != nil {
				res.event.ResponseFinish(nil)
			}
		} else {
			res.err = err
			if res.event != nil {
				res.event.ResponseFinish(err)
			}
		}
		return n, err
	}
}

//Err 返回错误,无错误返回nil
func (res *RestResult) Err() error {
	return res.err
}

//JsonResult 将结果转为JSON字符串
func (res *RestResult) JsonResult(path ...string) *JsonResult {
	defer func() {
		if res.event != nil {
			res.event.ResponseCheck(res.err)
		}
	}()
	if res.err != nil {
		return NewJsonResultFromError(res.err)
	}
	body, err := ioutil.ReadAll(res)
	if err != nil {
		return NewJsonResultFromError(res.err)
	}
	bodyStr := string(body)
	if check, ok := res.build.(RestJsonResult); ok {
		res.err = check.CheckJsonResult(bodyStr)
		if res.err != nil {
			return NewJsonResultFromError(res.err)
		}
	}
	basePath := ""
	if path != nil {
		basePath = path[0]
	}
	return NewJsonResult(bodyStr, basePath)
}

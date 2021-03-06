package errorformatter

import (
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/sigurn/crc16"
	"golang.org/x/mod/modfile"
)

const (
	SEPARATOR       = '#'
	WITH_CALL_CHAIN = false
	SKIP            = 2
	FORMAT_TPL      = "%c%d:%s%c%s"
)

type Formatter struct {
	Include    []string
	Exclude    []string
	HttpStatus func(packageName string, funcName string) (int, bool)
	PCs        func(err error, pc []uintptr) (n int)
	Cause      func(err error) (tagetErr error)
	Chan       chan<- *ErrorCode
}

type CodeInfo struct {
	Code     string    `json:"code"`
	File     string    `json:"file"`
	Package  string    `json:"package"`
	Function string    `json:"function"`
	Line     string    `json:"line"`
	Msg      string    `json:"msg"`
	Cause    *CodeInfo `json:"cause"`
}

type ErrorCode struct {
	HttpStatus int    `json:"-"`
	Code       string `json:"code"`
	Msg        string `json:"msg"`
	cause      error  `json:"-"`
	CodeInfo   *CodeInfo
}

func (e *ErrorCode) Error() string {

	msg := fmt.Sprintf(FORMAT_TPL, SEPARATOR, e.HttpStatus, e.Code, SEPARATOR, e.Msg)
	return msg
}
func (e *ErrorCode) Cause() error { return e.cause }

//ParseMsg parse string to *ErrorCode
func (e *ErrorCode) ParseMsg(msg string) (ok bool) {
	ok = false
	if msg[0] != byte(SEPARATOR) {
		return
	}
	arr := strings.SplitN(msg, string(SEPARATOR), 3)
	if len(arr) < 3 {
		return
	}
	codeArr := strings.SplitN(arr[1], ":", 2)
	if len(codeArr) < 2 {
		return
	}
	httpStatus, err := strconv.Atoi(codeArr[0])
	if err != nil {
		return
	}
	e.HttpStatus = httpStatus
	e.Code = codeArr[1]
	e.Msg = arr[2]
	ok = true
	return
}

func (e *ErrorCode) TraceInfo() (traceList []*CodeInfo) {
	traceList = make([]*CodeInfo, 0)
	codeInfo := e.CodeInfo
	for {
		if codeInfo != nil {
			copyCodeInfo := &CodeInfo{
				Code:     codeInfo.Code,
				File:     codeInfo.File,
				Package:  codeInfo.Package,
				Function: codeInfo.Function,
				Line:     codeInfo.Line,
				Msg:      codeInfo.Msg,
			}
			traceList = append(traceList, copyCodeInfo)
			codeInfo = codeInfo.Cause
		} else {
			break
		}
	}

	return
}

func New(
	include []string,
	exclude []string,
	httpStatus func(packageName string, funcName string) (int, bool),
	pcs func(err error, pc []uintptr) (n int),
	cause func(err error) (tagetErr error),
	ch chan<- *ErrorCode,
) (formatter *Formatter) {
	formatter = &Formatter{
		Include:    include,
		Exclude:    exclude,
		HttpStatus: httpStatus,
		PCs:        pcs,
		Cause:      cause,
		Chan:       ch,
	}
	return
}

//Msg generate *ErrorCode from msg
func (formatter *Formatter) Msg(msg string, args ...int) (err *ErrorCode) {
	httpStatus := 500
	code := "000000000"
	if len(args) >= 2 {
		httpStatus = args[0]
		code = strconv.Itoa(args[1])
		err = &ErrorCode{
			HttpStatus: httpStatus,
			Code:       code,
			Msg:        msg,
		}
		return
	}
	if len(args) == 1 {
		httpStatus = args[0]
	}
	pcArr := make([]uintptr, 32) // at least 1 entry needed
	n := runtime.Callers(SKIP, pcArr)
	frames := runtime.CallersFrames(pcArr[:n])
	codeInfo := formatter.Frames(frames)
	codeInfo.Msg = msg
	if formatter.HttpStatus != nil {
		tmpHttpStatus, ok := formatter.HttpStatus(codeInfo.Package, codeInfo.Function)
		if ok {
			httpStatus = tmpHttpStatus
		}
	}
	err = &ErrorCode{
		HttpStatus: httpStatus,
		Code:       codeInfo.Code,
		Msg:        msg,
		CodeInfo:   codeInfo,
	}
	formatter.SendToChain(err)
	return
}
func (formatter *Formatter) GenerateError(httpStatus int, businessCode string, msg string) (err error) {
	err = errors.Errorf(FORMAT_TPL, SEPARATOR, httpStatus, businessCode, SEPARATOR, msg)
	return
}

//Error generate *ErrorCode from error
func (formatter *Formatter) WrapError(err error) (newErr *ErrorCode) {
	if err == nil {
		return nil
	}
	if formatter.Cause != nil {
		err = formatter.Cause(err)
	}
	e, ok := err.(*ErrorCode)
	if ok {
		return e
	}
	//	err = errors.WithStack(err)
	httpStatus := 500
	var pcArr [32]uintptr // at least 1 entry needed
	var frames *runtime.Frames
	n := 0
	if formatter.PCs != nil {
		n = formatter.PCs(err, pcArr[:])
	} else {
		n = runtime.Callers(SKIP, pcArr[:])

	}
	frames = runtime.CallersFrames(pcArr[:n])
	codeInfo := formatter.Frames(frames)
	msg := fmt.Sprintf("%s: %s", GetErrorType(err), err.Error()) // ??????error???????????????????????????????????????
	codeInfo.Msg = msg
	if formatter.HttpStatus != nil {
		tmpHttpStatus, ok := formatter.HttpStatus(codeInfo.Package, codeInfo.Function)
		if ok {
			httpStatus = tmpHttpStatus
		}
	}
	newErr = &ErrorCode{
		HttpStatus: httpStatus,
		Code:       codeInfo.Code,
		Msg:        msg,
		cause:      err,
		CodeInfo:   codeInfo,
	}
	formatter.SendToChain(newErr)
	return
}

// Frames generate *CodeInfo from frames
func (formatter *Formatter) Frames(frames *runtime.Frames) (codeInfo *CodeInfo) {
	root := &CodeInfo{}
	point := root
	codeInfo = root
	codeArr := make([]string, 0)
	msgArr := make([]string, 0)
	hasInclude := len(formatter.Include) > 0
	hasExclude := len(formatter.Exclude) > 0
	for {
		frame, hasNext := frames.Next()
		file := frame.File
		fullFuncName := frame.Function
		line := frame.Line
		if point.Code != "" {
			codeArr = append(codeArr, point.Code)
		}
		if point.Msg != "" {
			msgArr = append(msgArr, point.Msg)
		}
		var matched bool

		if !matched && hasInclude { //Include ???????????????????????????
			for _, keyword := range formatter.Include {
				matched = strings.Contains(fullFuncName, keyword)
				if matched {
					break
				}
			}
		}

		if !matched && hasExclude { //Exclude ??????????????????????????????
			for _, keyword := range formatter.Exclude {
				matched = strings.Contains(fullFuncName, keyword)
				if matched {
					break
				}
			}
		}

		if !hasInclude && !hasExclude {
			matched = true
		}
		if matched {
			point.Cause = formatter.FuncName2CodeInfo(file, fullFuncName, line)
			point = point.Cause
		}
		if !hasNext {
			break
		}
	}
	// msgArr???codeArr ????????????root?????????????????????????????????
	root.Msg = strings.Join(msgArr, ":") // ???????????????codeInfo,???????????????????????????????????????????????????????????????????????????code??????
	switch len(codeArr) {
	case 0:
		root.Code = "000000000"
	case 1:
		root.Code = codeArr[0]
	default:
		firstCode := codeArr[0]
		restCode := codeArr[1:]
		restCodeStr := strings.Join(restCode, ":")
		table := crc16.MakeTable(crc16.CRC16_MAXIM)
		codePrefix := crc16.Checksum([]byte(restCodeStr), table)
		root.Code = fmt.Sprintf("%05d%s", codePrefix, firstCode[5:])
	}
	cause := root.Cause
	if cause != nil {
		root.File = cause.File
		root.Package = cause.Package
		root.Function = cause.Function
		root.Line = cause.Line
	}
	codeInfo = root
	return
}

//FuncName2CodeInfo generate *CodeInfo from full function name
func (formatter *Formatter) FuncName2CodeInfo(file string, fullFuncName string, line int) (codeInfo *CodeInfo) {
	if fullFuncName == "" {
		return &CodeInfo{}
	}
	lastSlashIndex := strings.LastIndex(fullFuncName, "/")
	basename := fullFuncName
	if lastSlashIndex > -1 {
		basename = fullFuncName[lastSlashIndex:]
	}
	firstDotIndex := lastSlashIndex + strings.Index(basename, ".")
	packageName := fullFuncName[:firstDotIndex]
	funcName := fullFuncName[firstDotIndex+1:]
	table := crc16.MakeTable(crc16.CRC16_MAXIM)
	nameCrc := crc16.Checksum([]byte(basename), table)
	code := fmt.Sprintf("%05d%04d", nameCrc, line)
	codeInfo = &CodeInfo{
		Code:     code,
		File:     fmt.Sprintf("%s:%d", file, line),
		Package:  packageName,
		Function: funcName,
		Line:     strconv.Itoa(line),
	}
	return
}

//FuncName2CodeInfo generate *CodeInfo from full function name
func (formatter *Formatter) SendToChain(errorCode *ErrorCode) (err error) {
	if formatter.Chan == nil {
		return
	}
	formatter.Chan <- errorCode
	return
}

func GetErrorType(err error) string {
	if err == nil {
		return ""
	}
	for err != nil {
		cause, ok := err.(Causer)
		if !ok {
			break
		}
		err = cause.Cause()
	}
	rv := reflect.Indirect(reflect.ValueOf(err))
	rt := rv.Type()
	msg := fmt.Sprintf("%s.%s", rt.PkgPath(), rt.Name()) // ?????????????????????????????????????????????????????????
	return msg

}

//ModuleName help function, get mod package name from go.mod
func ModuleName(goModelfile string) (modName string, err error) {
	goModBytes, err := os.ReadFile(goModelfile)
	if err != nil {
		return
	}
	modName = modfile.ModulePath(goModBytes)
	return
}

type Causer interface {
	Cause() error
}

//GithubComPkgErrors github.com/pkg/errors package implementation
type GithubComPkgErrors struct{}
type GithubComPkgErrorsStackTracer interface {
	StackTrace() errors.StackTrace
}

//PCs implementation (*Formatter).PCs function
func (pkgErrors *GithubComPkgErrors) PCs(err error, pc []uintptr) (n int) {

	n = 0
	stackErr, ok := err.(GithubComPkgErrorsStackTracer)
	if ok {
		stack := stackErr.StackTrace()
		n = len(stack)
		for i, frame := range stack {
			pc[i] = uintptr(frame) - 1
		}
	} else { // ??????StackTrace ???????????????????????????
		n = runtime.Callers(SKIP+1, pc[:])
	}
	return n
}

//Cause implementation (*Formatter).Cause function
func (pkgErrors *GithubComPkgErrors) Cause(err error) error {
	targetErr := err

	for err != nil {
		cause, ok := err.(Causer)
		if !ok {
			break
		}
		err = cause.Cause()
		if err != nil {
			if code, ok := err.(*ErrorCode); ok {
				targetErr = code
			} else {
				pcArr := make([]uintptr, 32)
				n := pkgErrors.PCs(err, pcArr)
				if n > 0 {
					targetErr = err
				}
			}
		}
	}
	return targetErr
}

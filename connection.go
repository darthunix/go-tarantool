package tnt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"time"
)

type Options struct {
	ConnectTimeout time.Duration
	QueryTimeout   time.Duration
	DefaultSpace   string
	User           string
	Password       string
}

type Greeting struct {
	Version []byte
	Auth    []byte
}

type Connection struct {
	addr        string
	requestID   uint32
	requests    map[uint32]*request
	requestChan chan *request
	closeOnce   sync.Once
	exit        chan bool
	closed      chan bool
	tcpConn     net.Conn
	// options
	queryTimeout time.Duration
	defaultSpace string
	Greeting     *Greeting
}

func Connect(addr string, options *Options) (conn *Connection, err error) {
	defer func() { // close opened connection if error
		if err != nil && conn != nil {
			if conn.tcpConn != nil {
				conn.tcpConn.Close()
			}
			conn = nil
		}
	}()

	conn = &Connection{
		addr:        addr,
		requests:    make(map[uint32]*request),
		requestChan: make(chan *request, 16),
		exit:        make(chan bool),
		closed:      make(chan bool),
	}

	if options == nil {
		options = &Options{}
	}

	opts := *options // copy to new object

	if opts.ConnectTimeout.Nanoseconds() == 0 {
		opts.ConnectTimeout = time.Duration(time.Second)
	}

	if opts.QueryTimeout.Nanoseconds() == 0 {
		opts.QueryTimeout = time.Duration(time.Second)
	}

	splittedAddr := strings.Split(addr, "/")
	remoteAddr := splittedAddr[0]

	if opts.DefaultSpace == "" {
		if len(splittedAddr) > 1 {
			if splittedAddr[1] == "" {
				return nil, fmt.Errorf("Wrong space: %s", splittedAddr[1])
			}
			options.DefaultSpace = splittedAddr[1]
		}
	}

	conn.queryTimeout = opts.QueryTimeout
	conn.defaultSpace = opts.DefaultSpace

	connectDeadline := time.Now().Add(opts.ConnectTimeout)

	conn.tcpConn, err = net.DialTimeout("tcp", remoteAddr, opts.ConnectTimeout)
	if err != nil {
		return nil, err
	}

	greeting := make([]byte, 128)

	conn.tcpConn.SetDeadline(connectDeadline)
	_, err = io.ReadFull(conn.tcpConn, greeting)
	if err != nil {
		return
	}

	conn.Greeting = &Greeting{
		Version: greeting[:64],
		Auth:    greeting[64:108],
	}

	if options.User != "" {
		var authRaw []byte
		var authReplyBody []byte
		var authResponse *Response

		authRequestID := conn.nextID()

		authRaw, err = (&Auth{
			User:         options.User,
			Password:     options.Password,
			GreetingAuth: conn.Greeting.Auth,
		}).Pack(authRequestID, "")

		_, err = conn.tcpConn.Write(authRaw)
		if err != nil {
			return
		}

		authReplyBody, err = readMessage(conn.tcpConn)
		if err != nil {
			return
		}

		authResponse, err = decodeResponse(bytes.NewBuffer(authReplyBody))
		if err != nil {
			return
		}

		if authResponse.requestID != authRequestID {
			err = errors.New("Bad auth responseID")
			return
		}

		if authResponse.Error != nil {
			err = authResponse.Error
			return
		}

	}

	conn.tcpConn.SetDeadline(time.Time{})

	go conn.worker(conn.tcpConn)

	return
}

func (conn *Connection) nextID() uint32 {
	if conn.requestID == math.MaxUint32 {
		conn.requestID = 0
	}
	conn.requestID++
	return conn.requestID
}

func (conn *Connection) newRequest(r *request) error {
	requestID := conn.nextID()
	old, exists := conn.requests[requestID]
	if exists {
		old.replyChan <- &Response{
			Error: NewConnectionError("Shred old requests"), // wtf?
		}
		close(old.replyChan)
		delete(conn.requests, requestID)
	}

	// pp.Println(r)
	var err error

	r.raw, err = r.query.Pack(requestID, conn.defaultSpace)
	if err != nil {
		r.replyChan <- &Response{
			Error: &QueryError{
				error: err,
			},
		}
		return err
	}

	conn.requests[requestID] = r

	return nil
}

func (conn *Connection) handleReply(res *Response) {
	request, exists := conn.requests[res.requestID]
	if exists {
		request.replyChan <- res
		close(request.replyChan)
		delete(conn.requests, res.requestID)
	}
}

func (conn *Connection) stop() {
	conn.closeOnce.Do(func() {
		// debug.PrintStack()
		close(conn.exit)
		conn.tcpConn.Close()
	})
}

func (conn *Connection) Close() {
	conn.stop()
	<-conn.closed
}

func (conn *Connection) worker(tcpConn net.Conn) {

	var wg sync.WaitGroup

	readChan := make(chan *Response, 256)
	writeChan := make(chan *request, 256)

	wg.Add(3)

	go func() {
		conn.router(readChan, writeChan, conn.exit)
		conn.stop()
		wg.Done()
		// pp.Println("router")
	}()

	go func() {
		writer(tcpConn, writeChan, conn.exit)
		conn.stop()
		wg.Done()
		// pp.Println("writer")
	}()

	go func() {
		reader(tcpConn, readChan)
		conn.stop()
		wg.Done()
		// pp.Println("reader")
	}()

	wg.Wait()

	// send error reply to all pending requests
	for requestID, req := range conn.requests {
		req.replyChan <- &Response{
			Error: ConnectionClosedError(),
		}
		close(req.replyChan)
		delete(conn.requests, requestID)
	}

	var req *request

FETCH_INPUT:
	// and to all requests in input queue
	for {
		select {
		case req = <-conn.requestChan:
			// pass
		default: // all fetched
			break FETCH_INPUT
		}
		req.replyChan <- &Response{
			Error: ConnectionClosedError(),
		}
		close(req.replyChan)
	}

	close(conn.closed)
}

func (conn *Connection) router(readChan chan *Response, writeChan chan *request, stopChan chan bool) {
	// close(readChan) for stop router
	requestChan := conn.requestChan

	readChanThreshold := cap(readChan) / 10

ROUTER_LOOP:
	for {
		// force read reply
		if len(readChan) > readChanThreshold {
			requestChan = nil
		} else {
			requestChan = conn.requestChan
		}

		select {
		case r, ok := <-requestChan:
			if !ok {
				break ROUTER_LOOP
			}

			if conn.newRequest(r) == nil { // already replied to errored requests
				select {
				case writeChan <- r:
					// pass
				case <-stopChan:
					break ROUTER_LOOP
				}
			}
		case <-stopChan:
			break ROUTER_LOOP
		case res, ok := <-readChan:
			if !ok {
				break ROUTER_LOOP
			}
			conn.handleReply(res)
		}
	}
}

func writer(tcpConn net.Conn, writeChan chan *request, stopChan chan bool) {
	var err error
WRITER_LOOP:
	for {
		select {
		case request, ok := <-writeChan:
			if !ok {
				break WRITER_LOOP
			}
			_, err = tcpConn.Write(request.raw)
			// @TODO: handle error
			if err != nil {
				break WRITER_LOOP
			}
		case <-stopChan:
			break WRITER_LOOP
		}
	}
	if err != nil {
		// @TODO
		// pp.Println(err)
	}
}

func readMessage(r io.Reader) ([]byte, error) {
	var err error
	header := make([]byte, PacketLengthBytes)

	if _, err = io.ReadAtLeast(r, header, PacketLengthBytes); err != nil {
		return nil, err
	}

	if header[0] != 0xce {
		return nil, errors.New("Wrong reponse header")
	}

	bodyLength := (int(header[1]) << 24) +
		(int(header[2]) << 16) +
		(int(header[3]) << 8) +
		int(header[4])

	if bodyLength == 0 {
		return nil, errors.New("Response should not be 0 length")
	}

	body := make([]byte, bodyLength)
	_, err = io.ReadAtLeast(r, body, bodyLength)
	if err != nil {
		return nil, err
	}

	return body, nil
}

func reader(tcpConn net.Conn, readChan chan *Response) {
	// var msgLen uint32
	// var err error
	header := make([]byte, 12)
	headerLen := len(header)

	var bodyLen uint32
	var requestID uint32
	var response *Response

	var err error

READER_LOOP:
	for {
		_, err = io.ReadAtLeast(tcpConn, header, headerLen)
		// @TODO: log error
		if err != nil {
			break READER_LOOP
		}

		// bodyLen = UnpackInt(header[4:8])
		// requestID = UnpackInt(header[8:12])

		body := make([]byte, bodyLen)

		_, err = io.ReadAtLeast(tcpConn, body, int(bodyLen))
		// @TODO: log error
		if err != nil {
			break READER_LOOP
		}

		// response, err = UnpackBody(body)
		response = nil
		// @TODO: log error
		if err != nil {
			break READER_LOOP
		}
		response.requestID = requestID

		readChan <- response
	}
}

func packIproto(requestCode byte, requestID uint32, body []byte) []byte {
	h := [...]byte{
		0xce, 0, 0, 0, 0, // length
		0x82,                       // 2 element map
		KeyCode, byte(requestCode), // request code
		KeySync, 0xce,
		byte(requestID >> 24), byte(requestID >> 16),
		byte(requestID >> 8), byte(requestID),
	}

	l := uint32(len(h) - 5 + len(body))
	h[1] = byte(l >> 24)
	h[2] = byte(l >> 16)
	h[3] = byte(l >> 8)
	h[4] = byte(l)

	return append(h[:], body...)
}
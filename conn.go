package amqp

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"time"

	"github.com/pkg/errors"
)

// connection defaults
const (
	initialMaxFrameSize = 512
	initialChannelMax   = 1
)

type Opt func(*Conn) error

func OptHostname(hostname string) Opt {
	return func(c *Conn) error {
		c.hostname = hostname
		return nil
	}
}

type stateFunc func() stateFunc

type Conn struct {
	net net.Conn

	maxFrameSize uint32
	channelMax   uint16
	hostname     string
	idleTimeout  time.Duration

	rxBuf []byte
	err   error

	// SASL
	saslHandlers map[Symbol]stateFunc
	saslComplete bool

	// mux
	readErr    chan error
	rxFrame    chan []byte
	txFrame    chan *bytes.Buffer
	newSession chan *Session
	delSession chan *Session
}

func Dial(addr string, opts ...Opt) (*Conn, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return nil, err
	}

	var conn net.Conn
	switch u.Scheme {
	case "amqp", "":
		conn, err = net.Dial("tcp", u.Host)
	default:
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}

	if err != nil {
		return nil, err
	}

	return New(conn, opts...)
}

func New(conn net.Conn, opts ...Opt) (*Conn, error) {
	c := &Conn{
		net:          conn,
		maxFrameSize: initialMaxFrameSize,
		channelMax:   initialChannelMax,
		idleTimeout:  1 * time.Minute,
		readErr:      make(chan error),
		rxFrame:      make(chan []byte),
		txFrame:      make(chan *bytes.Buffer),
		newSession:   make(chan *Session),
		delSession:   make(chan *Session),
	}

	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	for state := c.negotiateProto; state != nil; {
		state = state()
	}

	if c.err != nil && c.net != nil {
		c.net.Close()
	}

	return c, c.err
}

func (c *Conn) Close() error {
	// TODO: shutdown AMQP
	return c.net.Close()
}

func (c *Conn) MaxFrameSize() int {
	return int(c.maxFrameSize)
}

func (c *Conn) ChannelMax() int {
	return int(c.channelMax)
}

type Session struct {
	channel       uint16
	remoteChannel uint16
	conn          *Conn
	err           error
	rx            chan *frame

	newLink chan *link
	delLink chan *link
}

type frame struct {
	header  frameHeader
	payload []byte
}

func (s *Session) Close() {
	// TODO: send end preformative
	s.conn.delSession <- s
}

func (s *Session) begin() error {
	begin, err := Marshal(&Begin{
		NextOutgoingID: 0,
		IncomingWindow: 1,
	})

	if err != nil {
		return err
	}

	wr := bufPool.New().(*bytes.Buffer)
	wr.Reset()

	err = writeFrame(wr, FrameTypeAMQP, s.channel, begin)
	if err != nil {
		return err
	}

	s.conn.txFrame <- wr
	fr := <-s.rx

	pType, err := preformativeType(fr.payload)
	if err != nil {
		return err
	}

	if pType != PreformativeBegin {
		return fmt.Errorf("unexpected begin response: %+v", fr)
	}

	var resp Begin
	err = Unmarshal(bytes.NewReader(fr.payload), &resp)
	if err != nil {
		return err
	}

	fmt.Printf("Begin Resp: %+v", resp)
	// TODO: record negotiated settings
	s.remoteChannel = fr.header.channel

	return nil
}

type link struct {
	handle     uint32
	sourceAddr string
	linkCredit uint32
	rx         chan interface{}

	creditUsed          uint32
	senderDeliveryCount uint32
}

func newLink(handle uint32) *link {
	return &link{
		handle:     handle,
		linkCredit: 1,
		rx:         make(chan interface{}),
	}
}

type LinkOption func(*link) error

func LinkSource(source string) LinkOption {
	return func(l *link) error {
		l.sourceAddr = source
		return nil
	}
}

func LinkCredit(credit uint32) LinkOption {
	return func(l *link) error {
		l.linkCredit = credit
		return nil
	}
}

type Receiver struct {
	link    *link
	session *Session

	buf *bytes.Buffer
}

func (s *Session) Receiver(opts ...LinkOption) (*Receiver, error) {
	link := <-s.newLink

	for _, o := range opts {
		err := o(link)
		if err != nil {
			// TODO: release link
			return nil, err
		}
	}
	link.creditUsed = link.linkCredit

	attach, err := Marshal(&Attach{
		Name:   "ASHJDJKHJA-ASDHJ-ASDHGJH-ASDSAD78Y",
		Handle: link.handle,
		Role:   true,
		Source: &Source{
			Address: link.sourceAddr,
		},
	})
	if err != nil {
		return nil, err
	}

	wr := bufPool.New().(*bytes.Buffer)
	wr.Reset()
	err = writeFrame(wr, FrameTypeAMQP, s.channel, attach)
	if err != nil {
		return nil, err
	}
	s.conn.txFrame <- wr

	fr := <-link.rx

	resp, ok := fr.(*Attach)
	if !ok {
		return nil, fmt.Errorf("unexpected attach response: %+v", fr)
	}

	fmt.Printf("Attach Resp: %+v\n", resp)
	fmt.Printf("Attach Source: %+v\n", resp.Source)
	fmt.Printf("Attach Target: %+v\n", resp.Target)

	link.senderDeliveryCount = resp.InitialDeliveryCount

	r := &Receiver{
		link:    link,
		session: s,
		buf:     bufPool.New().(*bytes.Buffer),
	}

	return r, nil
}

func (r *Receiver) sendFlow() error {
	newLinkCredit := r.link.linkCredit - (r.link.linkCredit - r.link.creditUsed)
	r.link.senderDeliveryCount += r.link.creditUsed
	flow, err := Marshal(&Flow{
		IncomingWindow: 2147483647,
		NextOutgoingID: 0,
		OutgoingWindow: 0,
		Handle:         &r.link.handle,
		DeliveryCount:  &r.link.senderDeliveryCount,
		LinkCredit:     &newLinkCredit,
	})
	if err != nil {
		return err
	}

	fmt.Printf("WRITING FLOW: %+v\n", flow)

	wr := bufPool.New().(*bytes.Buffer)
	wr.Reset()
	err = writeFrame(wr, FrameTypeAMQP, r.session.channel, flow)
	if err != nil {
		return err
	}
	r.session.conn.txFrame <- wr
	r.link.creditUsed = 0
	return nil
}

func (m *Message) sendDisposition(state interface{}) error {
	disp, err := Marshal(&Disposition{
		Role:    true,
		First:   m.deliveryID,
		Settled: true,
		State:   state,
	})
	if err != nil {
		return err
	}

	wr := bufPool.New().(*bytes.Buffer)
	wr.Reset()
	err = writeFrame(wr, FrameTypeAMQP, m.r.session.channel, disp)
	if err != nil {
		return err
	}
	m.r.session.conn.txFrame <- wr
	return nil
}

// Accept notifies the server that the message has been
// accepted and does not require redelivery.
func (m *Message) Accept() error {
	return m.sendDisposition(&StateAccepted{})
}

// Reject notifies the server that the message is invalid
func (m *Message) Reject() error {
	return m.sendDisposition(&StateRejected{})
}

func (m *Message) Release() error {
	return m.sendDisposition(&StateReleased{})
}

func (r *Receiver) Receive() (*Message, error) {
	r.buf.Reset()

	msg := &Message{r: r}

	first := true
	for {
		if r.link.creditUsed > r.link.linkCredit/2 {
			if err := r.sendFlow(); err != nil {
				return nil, err
			}
		}

		fr := <-r.link.rx
		transfer := fr.(*Transfer)
		r.link.creditUsed++
		fmt.Printf("Transfer frame: %+v\n", transfer)
		if first && transfer.DeliveryID != nil {
			msg.deliveryID = *transfer.DeliveryID
			first = false
		}
		r.buf.Write(transfer.Payload)
		if !transfer.More {
			break
		}
	}

	err := Unmarshal(r.buf, msg)
	if err != nil {
		return msg, err
	}

	return msg, err
}

func (r *Receiver) Close() error {
	bufPool.Put(r.buf)
	r.buf = nil
	r.session.delLink <- r.link
	return nil
}

func (c *Conn) Session() (*Session, error) {
	s := <-c.newSession
	if s.err != nil {
		return nil, s.err
	}

	err := s.begin()
	if err != nil {
		s.Close()
		return nil, err
	}

	s.newLink = make(chan *link)
	s.delLink = make(chan *link)

	go s.startMux()

	return s, nil
}

func (s *Session) startMux() {
	links := make(map[uint32]*link)
	nextLink := newLink(0)
	for {
		select {
		case s.newLink <- nextLink:
			fmt.Println("Got new link request")
			links[nextLink.handle] = nextLink
			// TODO: handle max session/wrapping
			nextLink = newLink(nextLink.handle + 1)
		case link := <-s.delLink:
			fmt.Println("Got link deletion request")
			delete(links, link.handle)
		case fr := <-s.rx:
			go func() {
				pType, err := preformativeType(fr.payload)
				if err != nil {
					log.Println("error:", err)
					return
				}

				switch pType {
				case PreformativeAttach:
					var attach Attach
					err = Unmarshal(bytes.NewReader(fr.payload), &attach)
					if err != nil {
						log.Println("error:", err)
						return
					}
					link, ok := links[attach.Handle]
					if ok {
						link.rx <- &attach
					}
					// TODO: error
				case PreformativeTransfer:
					var transfer Transfer
					err = Unmarshal(bytes.NewReader(fr.payload), &transfer)
					if err != nil {
						log.Println("error:", err)
						return
					}
					link, ok := links[transfer.Handle]
					if ok {
						link.rx <- &transfer
					}
				default:
					// TODO: error
					fmt.Printf("frame: %#v\n", fr)
				}
			}()
		}
	}
}

func (c *Conn) startMux() {
	go c.connReader()

	nextSession := &Session{conn: c, rx: make(chan *frame)}

	// map channel to session
	sessions := make(map[uint16]*Session)

	keepalive := time.NewTicker(c.idleTimeout / 2)
	var buf bytes.Buffer
	writeFrame(&buf, FrameTypeAMQP, 0, nil)
	keepaliveFrame := buf.Bytes()
	buf.Reset()

	fmt.Println("Starting mux")

outer:
	for {
		if c.err != nil {
			panic(c.err) // TODO: graceful close
		}

		select {
		case err := <-c.readErr:
			fmt.Println("Got read error")
			c.err = err

		case rawFrame := <-c.rxFrame:
			_, err := buf.Write(rawFrame)
			if err != nil {
				c.err = err
				continue
			}
			fmt.Println("Got rxFrame")
			for buf.Len() > 8 { // 8 = min size for header
				frameHeader, err := parseFrameHeader(buf.Bytes())
				if err != nil {
					c.err = err
					continue outer
				}

				ch, ok := sessions[frameHeader.channel]
				if !ok {
					c.err = errors.Errorf("unexpected frame header: %+v", frameHeader)
					continue outer
				}

				fmt.Printf("%+v, %d\n\n", frameHeader, buf.Len())

				if buf.Len() < int(frameHeader.size) {
					continue outer
				}

				payload := make([]byte, frameHeader.size)
				_, err = io.ReadFull(&buf, payload)
				if err != nil {
					c.err = err
					continue outer
				}

				ch.rx <- &frame{header: frameHeader, payload: payload[8:]}
			}

		case c.newSession <- nextSession:
			fmt.Println("Got new session request")
			sessions[nextSession.channel] = nextSession
			// TODO: handle max session/wrapping
			nextSession = &Session{conn: c, channel: nextSession.channel + 1, rx: make(chan *frame)}

		case s := <-c.delSession:
			fmt.Println("Got delete session request")
			delete(sessions, s.channel)

		case fr := <-c.txFrame:
			fmt.Printf("Writing: %# 02x\n", fr)
			_, c.err = c.net.Write(fr.Bytes())
			bufPool.Put(fr)
		case <-keepalive.C:
			fmt.Printf("Writing: %# 02x\n", keepaliveFrame)
			_, c.err = c.net.Write(keepaliveFrame)
		}
	}
}

func (c *Conn) connReader() {
	for {
		n, err := c.net.Read(c.rxBuf[:c.maxFrameSize]) // TODO: send error on frame too large
		if err != nil {
			c.readErr <- err
			return
		}

		c.rxFrame <- append([]byte(nil), c.rxBuf[:n]...)
	}
}

/*
On connection open, we'll need to handle 4 possible scenarios:
1. Straight into AMQP.
2. SASL -> AMQP.
3. TLS -> AMQP.
4. TLS -> SASL -> AMQP
*/
func (c *Conn) negotiateProto() stateFunc {
	switch {
	case c.saslHandlers != nil && !c.saslComplete:
		return c.exchangeProtoHeader(ProtoSASL)
	default:
		return c.exchangeProtoHeader(ProtoAMQP)
	}
}

// ProtoIDs
const (
	ProtoAMQP = 0x0
	ProtoTLS  = 0x2
	ProtoSASL = 0x3
)

func (c *Conn) exchangeProtoHeader(proto uint8) stateFunc {
	_, c.err = c.net.Write([]byte{'A', 'M', 'Q', 'P', proto, 1, 0, 0})
	if c.err != nil {
		return nil
	}

	c.rxBuf = make([]byte, c.maxFrameSize)
	n, err := c.net.Read(c.rxBuf)
	if err != nil {
		c.err = err
		return nil
	}

	fmt.Printf("Read %d bytes.\n", n)

	p, err := parseProto(c.rxBuf[:n])
	if err != nil {
		c.err = err
		return nil
	}

	fmt.Printf("Proto: %s; ProtoID: %d; Version: %d.%d.%d\n",
		p.proto,
		p.protoID,
		p.major,
		p.minor,
		p.revision,
	)

	if proto != p.protoID {
		c.err = fmt.Errorf("unexpected protocol header %#00x, expected %#00x", p.protoID, proto)
		return nil
	}

	switch proto {
	case ProtoAMQP:
		return c.txOpen
	case ProtoTLS:
		// TODO
		return nil
	case ProtoSASL:
		return c.protoSASL
	default:
		c.err = fmt.Errorf("unknown protocol ID %#02x", p.protoID)
		return nil
	}
}

func (c *Conn) txOpen() stateFunc {
	open, err := Marshal(&Open{
		ContainerID:  "gopher",
		Hostname:     c.hostname,
		MaxFrameSize: c.maxFrameSize,
		ChannelMax:   c.channelMax,
	})
	if err != nil {
		c.err = err
		return nil
	}

	wr := bufPool.New().(*bytes.Buffer)
	wr.Reset()
	defer bufPool.Put(wr)

	writeFrame(wr, FrameTypeAMQP, 0, open)

	fmt.Printf("Writing: %# 02x\n", wr.Bytes())

	_, err = c.net.Write(wr.Bytes())
	if err != nil {
		c.err = errors.Wrapf(err, "writing")
		return nil
	}

	return c.rxOpen
}

func (c *Conn) rxOpen() stateFunc {
	n, err := c.net.Read(c.rxBuf)
	if err != nil {
		c.err = errors.Wrapf(err, "reading")
		return nil
	}

	fh, err := parseFrameHeader(c.rxBuf[:n])
	if err != nil {
		c.err = errors.Wrapf(err, "parsing frame header")
		return nil
	}

	if fh.frameType != FrameTypeAMQP {
		c.err = fmt.Errorf("unexpected frame type %#02x", fh.frameType)
	}

	var o Open
	err = Unmarshal(bytes.NewBuffer(c.rxBuf[fh.dataOffsetBytes():n]), &o)
	if err != nil {
		c.err = errors.Wrapf(err, "unmarshaling")
		return nil
	}

	fmt.Printf("Rx Open: %#v\n", o)

	if o.IdleTimeout.Duration > 0 {
		c.idleTimeout = o.IdleTimeout.Duration
	}

	if o.MaxFrameSize < c.maxFrameSize {
		c.maxFrameSize = o.MaxFrameSize
	}
	if o.ChannelMax < c.channelMax {
		c.channelMax = o.ChannelMax
	}

	if uint32(len(c.rxBuf)) < c.maxFrameSize {
		c.rxBuf = make([]byte, c.maxFrameSize)
	}

	go c.startMux()

	return nil
}

func (c *Conn) protoSASL() stateFunc {
	if c.saslHandlers == nil {
		// we don't support SASL
		c.err = fmt.Errorf("server request SASL, but not configured")
		return nil
	}

	n, err := c.net.Read(c.rxBuf)
	if err != nil {
		c.err = err
		return nil
	}

	fh, err := parseFrameHeader(c.rxBuf[:n])
	if err != nil {
		c.err = err
		return nil
	}

	if fh.frameType != FrameTypeSASL {
		c.err = fmt.Errorf("unexpected frame type %#02x", fh.frameType)
	}

	var sm SASLMechanisms
	err = Unmarshal(bytes.NewBuffer(c.rxBuf[fh.dataOffsetBytes():n]), &sm)
	if err != nil {
		c.err = err
		return nil
	}

	for _, mech := range sm.Mechanisms {
		if state, ok := c.saslHandlers[mech]; ok {
			return state
		}
	}

	// TODO: send some sort of "auth not supported" frame?
	c.err = fmt.Errorf("no supported auth mechanism (%v)", sm.Mechanisms)
	return nil
}

func (c *Conn) saslOutcome() stateFunc {
	n, err := c.net.Read(c.rxBuf)
	if err != nil {
		c.err = err
		return nil
	}

	fh, err := parseFrameHeader(c.rxBuf[:n])
	if err != nil {
		c.err = err
		return nil
	}

	if fh.frameType != FrameTypeSASL {
		c.err = fmt.Errorf("unexpected frame type %#02x", fh.frameType)
	}

	var so SASLOutcome
	c.err = Unmarshal(bytes.NewBuffer(c.rxBuf[fh.dataOffsetBytes():n]), &so)
	if c.err != nil {
		return nil
	}

	if so.Code != CodeSASLOK {
		c.err = fmt.Errorf("SASL PLAIN auth failed with code %#00x: %s", so.Code, so.AdditionalData)
		return nil
	}

	c.saslComplete = true

	return c.negotiateProto
}

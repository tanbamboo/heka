/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is the Mozilla Foundation.
# Portions created by the Initial Developer are Copyright (C) 2012
# the Initial Developer. All Rights Reserved.
#
# Contributor(s):
#   Rob Miller (rmiller@mozilla.com)
#
# ***** END LICENSE BLOCK *****/
package pipeline

import (
	"code.google.com/p/gomock/gomock"
	"code.google.com/p/goprotobuf/proto"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"github.com/mozilla-services/heka/message"
	ts "github.com/mozilla-services/heka/testsupport"
	gs "github.com/rafrombrc/gospec/src/gospec"
	"net"
	"sync"
	"time"
)

type InputTestHelper struct {
	Msg             *message.Message
	Pack            *PipelinePack
	AddrStr         string
	ResolvedAddrStr string
	MockHelper      *MockPluginHelper
	MockInputRunner *MockInputRunner
	Decoders        []DecoderRunner
	PackSupply      chan *PipelinePack
	DecodeChan      chan *PipelinePack
}

type PanicInput struct{}

func (p *PanicInput) Init(config interface{}) (err error) {
	return
}

func (p *PanicInput) Run(ir InputRunner, h PluginHelper) (err error) {
	panic("PANICINPUT")
}

func (p *PanicInput) Stop() {
	panic("PANICINPUT")
}

func getPayloadBytes(hbytes, mbytes []byte) func(msgBytes []byte) {
	return func(msgBytes []byte) {
		msgBytes[0] = message.RECORD_SEPARATOR
		msgBytes[1] = uint8(len(hbytes))
		copy(msgBytes[2:], hbytes)
		pos := 2 + len(hbytes)
		msgBytes[pos] = message.UNIT_SEPARATOR
		copy(msgBytes[pos+1:], mbytes)
	}
}

func InputsSpec(c gs.Context) {
	t := &ts.SimpleT{}
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ith := new(InputTestHelper)
	ith.Msg = getTestMessage()
	ith.Pack = getTestPipelinePack()

	// Specify localhost, but we're not really going to use the network
	ith.AddrStr = "localhost:55565"
	ith.ResolvedAddrStr = "127.0.0.1:55565"

	// set up mock helper, decoder set, and packSupply channel
	ith.MockHelper = NewMockPluginHelper(ctrl)
	ith.MockInputRunner = NewMockInputRunner(ctrl)
	ith.Decoders = make([]DecoderRunner, int(message.Header_JSON+1))
	ith.Decoders[message.Header_PROTOCOL_BUFFER] = NewMockDecoderRunner(ctrl)
	ith.Decoders[message.Header_JSON] = NewMockDecoderRunner(ctrl)
	ith.PackSupply = make(chan *PipelinePack, 1)
	ith.DecodeChan = make(chan *PipelinePack)
	key := "testkey"
	signers := map[string]Signer{"test_1": {key}}
	signer := "test"

	c.Specify("A UdpInput", func() {
		udpInput := UdpInput{}
		err := udpInput.Init(&UdpInputConfig{ith.AddrStr})
		c.Assume(err, gs.IsNil)
		realListener := (udpInput.listener).(*net.UDPConn)
		c.Expect(realListener.LocalAddr().String(), gs.Equals, ith.ResolvedAddrStr)
		realListener.Close()

		// replace the listener object w/ a mock listener
		mockListener := ts.NewMockConn(ctrl)
		udpInput.listener = mockListener

		msgJson, _ := json.Marshal(ith.Msg)
		putMsgJsonInBytes := func(msgBytes []byte) {
			copy(msgBytes, msgJson)
		}

		c.Specify("reads a message from the connection and passes it to the decoder", func() {
			ith.MockHelper.EXPECT().DecodersByEncoding().Return(ith.Decoders)
			readCall := mockListener.EXPECT().Read(ith.Pack.MsgBytes)
			readCall.Return(len(msgJson), nil)
			readCall.Do(putMsgJsonInBytes)

			mockDecoderRunner := ith.Decoders[message.Header_JSON].(*MockDecoderRunner)
			mockDecoderRunner.EXPECT().InChan().Return(ith.DecodeChan)
			ith.MockInputRunner.EXPECT().InChan().Times(2).Return(ith.PackSupply)

			// start the input
			go func() {
				udpInput.Run(ith.MockInputRunner, ith.MockHelper)
			}()
			ith.PackSupply <- ith.Pack
			packRef := <-ith.DecodeChan
			c.Expect(ith.Pack, gs.Equals, packRef)
			c.Expect(string(ith.Pack.MsgBytes), gs.Equals, string(msgJson))
			c.Expect(ith.Pack.Decoded, gs.IsFalse)
		})
	})

	c.Specify("A TcpInput", func() {
		tcpInput := TcpInput{}
		err := tcpInput.Init(&TcpInputConfig{ith.AddrStr, signers})
		c.Assume(err, gs.IsNil)
		realListener := tcpInput.listener
		c.Expect(realListener.Addr().String(), gs.Equals, ith.ResolvedAddrStr)
		realListener.Close()

		mockConnection := ts.NewMockConn(ctrl)
		mockListener := ts.NewMockListener(ctrl)
		tcpInput.listener = mockListener

		mbytes, _ := proto.Marshal(ith.Msg)
		header := &message.Header{}
		header.SetMessageLength(uint32(len(mbytes)))
		buf := make([]byte, message.MAX_MESSAGE_SIZE+message.MAX_HEADER_SIZE)
		err = errors.New("connection closed")
		readCall := mockConnection.EXPECT().Read(buf)
		ith.MockHelper.EXPECT().DecodersByEncoding().Return(ith.Decoders)

		neterr := ts.NewMockError(ctrl)
		neterr.EXPECT().Temporary().Return(false)
		acceptCall := mockListener.EXPECT().Accept().Return(mockConnection, nil)
		acceptCall.Do(func() {
			acceptCall = mockListener.EXPECT().Accept()
			acceptCall.Return(nil, neterr)
		})

		mockDecoderRunner := ith.Decoders[message.Header_PROTOCOL_BUFFER].(*MockDecoderRunner)
		mockDecoderRunner.EXPECT().InChan().Return(ith.DecodeChan)
		ith.MockInputRunner.EXPECT().InChan().Return(ith.PackSupply)

		c.Specify("reads a message from its connection", func() {
			hbytes, _ := proto.Marshal(header)
			buflen := 3 + len(hbytes) + len(mbytes)
			readCall.Return(buflen, err)
			readCall.Do(getPayloadBytes(hbytes, mbytes))
			go func() {
				tcpInput.Run(ith.MockInputRunner, ith.MockHelper)
			}()
			ith.PackSupply <- ith.Pack
			packRef := <-ith.DecodeChan
			c.Expect(ith.Pack, gs.Equals, packRef)
			c.Expect(string(ith.Pack.MsgBytes), gs.Equals, string(mbytes))
		})

		c.Specify("reads a MD5 signed message from its connection", func() {
			header.SetHmacHashFunction(message.Header_MD5)
			header.SetHmacSigner(signer)
			header.SetHmacKeyVersion(uint32(1))
			hm := hmac.New(md5.New, []byte(key))
			hm.Write(mbytes)
			header.SetHmac(hm.Sum(nil))
			hbytes, _ := proto.Marshal(header)
			buflen := 3 + len(hbytes) + len(mbytes)
			readCall.Return(buflen, err)
			readCall.Do(getPayloadBytes(hbytes, mbytes))

			go func() {
				tcpInput.Run(ith.MockInputRunner, ith.MockHelper)
			}()
			ith.PackSupply <- ith.Pack
			timeout := make(chan bool, 1)
			go func() {
				time.Sleep(100 * time.Millisecond)
				timeout <- true
			}()
			select {
			case packRef := <-ith.DecodeChan:
				c.Expect(ith.Pack, gs.Equals, packRef)
				c.Expect(string(ith.Pack.MsgBytes), gs.Equals, string(mbytes))
				c.Expect(ith.Pack.Signer, gs.Equals, "test")
			case t := <-timeout:
				c.Expect(t, gs.IsNil)
			}
		})

		c.Specify("reads a SHA1 signed message from its connection", func() {
			header.SetHmacHashFunction(message.Header_SHA1)
			header.SetHmacSigner(signer)
			header.SetHmacKeyVersion(uint32(1))
			hm := hmac.New(sha1.New, []byte(key))
			hm.Write(mbytes)
			header.SetHmac(hm.Sum(nil))
			hbytes, _ := proto.Marshal(header)
			buflen := 3 + len(hbytes) + len(mbytes)
			readCall.Return(buflen, err)
			readCall.Do(getPayloadBytes(hbytes, mbytes))

			go func() {
				tcpInput.Run(ith.MockInputRunner, ith.MockHelper)
			}()
			ith.PackSupply <- ith.Pack
			timeout := make(chan bool, 1)
			go func() {
				time.Sleep(100 * time.Millisecond)
				timeout <- true
			}()
			select {
			case packRef := <-ith.DecodeChan:
				c.Expect(ith.Pack, gs.Equals, packRef)
				c.Expect(string(ith.Pack.MsgBytes), gs.Equals, string(mbytes))
				c.Expect(ith.Pack.Signer, gs.Equals, "test")
			case t := <-timeout:
				c.Expect(t, gs.IsNil)
			}
		})

		c.Specify("reads a signed message with an expired key from its connection", func() {
			header.SetHmacHashFunction(message.Header_MD5)
			header.SetHmacSigner(signer)
			header.SetHmacKeyVersion(uint32(11)) // non-existent key version
			hm := hmac.New(md5.New, []byte(key))
			hm.Write(mbytes)
			header.SetHmac(hm.Sum(nil))
			hbytes, _ := proto.Marshal(header)
			buflen := 3 + len(hbytes) + len(mbytes)
			readCall.Return(buflen, err)
			readCall.Do(getPayloadBytes(hbytes, mbytes))

			go func() {
				tcpInput.Run(ith.MockInputRunner, ith.MockHelper)
			}()
			ith.PackSupply <- ith.Pack
			go func() {
				time.Sleep(100 * time.Millisecond)
				close(mockDecoderRunner.InChan())
			}()
			select {
			case packRef := <-ith.DecodeChan:
				c.Expect(packRef, gs.IsNil)
			}
		})

		c.Specify("reads a signed message with an incorrect hmac from its connection", func() {
			header.SetHmacHashFunction(message.Header_MD5)
			header.SetHmacSigner(signer)
			header.SetHmacKeyVersion(uint32(1))
			hm := hmac.New(md5.New, []byte(key))
			hm.Write([]byte("some bytes"))
			header.SetHmac(hm.Sum(nil))
			hbytes, _ := proto.Marshal(header)
			buflen := 3 + len(hbytes) + len(mbytes)
			readCall.Return(buflen, err)
			readCall.Do(getPayloadBytes(hbytes, mbytes))

			go func() {
				tcpInput.Run(ith.MockInputRunner, ith.MockHelper)
			}()
			ith.PackSupply <- ith.Pack
			go func() {
				time.Sleep(100 * time.Millisecond)
				close(mockDecoderRunner.InChan())
			}()
			select {
			case packRef := <-ith.DecodeChan:
				c.Expect(packRef, gs.IsNil)
			}
		})
	})

	c.Specify("Runner recovers from panic in input's `Run()` method", func() {
		input := new(PanicInput)
		iRunner := NewInputRunner("panic", input)
		var wg sync.WaitGroup
		ith.MockHelper.EXPECT().PackSupply()
		wg.Add(1)
		iRunner.Start(ith.MockHelper, &wg) // no panic => success
		wg.Wait()
	})
}

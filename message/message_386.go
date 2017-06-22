// Copyright 2016 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package message

import (
	"bytes"
	"encoding/binary"
	"errors"
	"unsafe"

	"github.com/vmware/vmw-guestinfo/bdoor"
)

const (
	messageTypeOpen = iota
	messageTypeSendSize
	messageTypeSendPayload
	messageTypeReceiveSize
	messageTypeReceivePayload
	messageTypeReceiveStatus
	messageTypeClose

	messageStatusFail       = uint16(0x0000)
	messageStatusSuccess    = uint16(0x0001)
	messageStatusDoRecieve  = uint16(0x0002)
	messageStatusCheckPoint = uint16(0x0010)
	messageStatusHighBW     = uint16(0x0080)
)

var (
	// ErrChannelOpen represents a failure to open a channel
	ErrChannelOpen = errors.New("could not open channel")
	// ErrChannelClose represents a failure to close a channel
	ErrChannelClose = errors.New("could not close channel")
	// ErrRpciSend represents a failure to send a message
	ErrRpciSend = errors.New("unable to send RPCI command")
	// ErrRpciReceive represents a failure to receive a message
	ErrRpciReceive = errors.New("unable to receive RPCI command result")
)

type Channel struct {
	id uint16

	forceLowBW bool
	buf        []byte

	cookie bdoor.UInt64
}

// NewChannel opens a new Channel
func NewChannel(proto uint32) (*Channel, error) {
	flags := bdoor.CommandFlagCookie

retry:
	bp := &bdoor.BackdoorProto{}

	bp.BX.SetWord(proto | flags)
	bp.CX.High = messageTypeOpen
	bp.CX.Low = bdoor.CommandMessage

	out := bp.InOut()
	if (out.CX.High & messageStatusSuccess) == 0 {
		if flags != 0 {
			flags = 0
			goto retry
		}

		Errorf("Message: Unable to open communication channel")
		return nil, ErrChannelOpen
	}

	ch := &Channel{}
	ch.id = out.DX.High
	ch.cookie.High.SetWord(out.SI.Word())
	ch.cookie.Low.SetWord(out.DI.Word())

	Debugf("Opened channel %d", ch.id)
	return ch, nil
}

func (c *Channel) Close() error {
	bp := &bdoor.BackdoorProto{}

	bp.CX.High = messageTypeClose
	bp.CX.Low = bdoor.CommandMessage

	bp.DX.High = c.id
	bp.SI.SetWord(c.cookie.High.Word())
	bp.DI.SetWord(c.cookie.Low.Word())

	out := bp.InOut()
	if (out.CX.High & messageStatusSuccess) == 0 {
		Errorf("Message: Unable to close communication channel %d", c.id)
		return ErrChannelClose
	}

	Debugf("Closed channel %d", c.id)
	return nil
}

func (c *Channel) Send(buf []byte) error {
retry:
	bp := &bdoor.BackdoorProto{}
	bp.CX.High = messageTypeSendSize
	bp.CX.Low = bdoor.CommandMessage

	bp.DX.High = c.id
	bp.SI.SetWord(c.cookie.High.Word())
	bp.DI.SetWord(c.cookie.Low.Word())

	bp.BX.SetWord(uint32(len(buf)))

	// send the size
	out := bp.InOut()
	if (out.CX.High & messageStatusSuccess) == 0 {
		Errorf("Message: Unable to send a message over the communication channel %d", c.id)
		return ErrRpciSend
	}

	// size of buf 0 is fine, just return
	if len(buf) == 0 {
		return nil
	}

	if !c.forceLowBW && (out.CX.High&messageStatusHighBW) == messageStatusHighBW {
		hbbp := &bdoor.BackdoorProto{}

		hbbp.BX.Low = bdoor.CommandHighBWMessage
		hbbp.BX.High = messageStatusSuccess
		hbbp.DX.High = c.id
		hbbp.BP.SetWord(c.cookie.High.Word())
		hbbp.DI.SetWord(c.cookie.Low.Word())
		hbbp.CX.SetWord(uint32(len(buf)))
		hbbp.SI.SetWord(uint32(uintptr(unsafe.Pointer(&buf[0]))))

		out := hbbp.HighBandwidthOut()
		if (out.BX.High & messageStatusSuccess) == 0 {
			if (out.BX.High & messageStatusCheckPoint) != 0 {
				Debugf("A checkpoint occurred. Retrying the operation")
				goto retry
			}

			Errorf("Message: Unable to send a message over the communication channel %d", c.id)
			return ErrRpciSend
		}
	} else {
		bp.CX.High = messageTypeSendPayload

		bbuf := bytes.NewBuffer(buf)
		for {
			// read 4 bytes at a time
			words := bbuf.Next(4)
			if len(words) == 0 {
				break
			}

			Debugf("sending %q over %d", string(words), c.id)
			switch len(words) {
			case 3:
				bp.BX.SetWord(binary.LittleEndian.Uint32([]byte{0x0, words[2], words[1], words[0]}))
			case 2:
				bp.BX.SetWord(uint32(binary.LittleEndian.Uint16(words)))
			case 1:
				bp.BX.SetWord(uint32(words[0]))
			default:
				bp.BX.SetWord(binary.LittleEndian.Uint32(words))
			}

			out = bp.InOut()
			if (out.CX.High & messageStatusSuccess) == 0 {
				Errorf("Message: Unable to send a message over the communication channel %d", c.id)
				return ErrRpciSend
			}
		}
	}

	return nil
}

func (c *Channel) Receive() ([]byte, error) {
retry:
	var err error
	bp := &bdoor.BackdoorProto{}
	bp.CX.High = messageTypeReceiveSize
	bp.CX.Low = bdoor.CommandMessage

	bp.DX.High = c.id
	bp.SI.SetWord(c.cookie.High.Word())
	bp.DI.SetWord(c.cookie.Low.Word())

	out := bp.InOut()
	if (out.CX.High & messageStatusSuccess) == 0 {
		Errorf("Message: Unable to poll for messages over the communication channel %d", c.id)
		return nil, ErrRpciReceive
	}

	if (out.CX.High & messageStatusDoRecieve) == 0 {
		Debugf("No message to retrieve")
		return nil, nil
	}

	// Receive the size.
	if out.DX.High != messageTypeSendSize {
		Errorf("Message: Protocol error. Expected a MESSAGE_TYPE_SENDSIZE request from vmware")
		return nil, ErrRpciReceive
	}

	size := out.BX.Word()

	var buf []byte

	if size != 0 {
		if !c.forceLowBW && (out.CX.High&messageStatusHighBW == messageStatusHighBW) {
			buf = make([]byte, size)

			hbbp := &bdoor.BackdoorProto{}

			hbbp.BX.Low = bdoor.CommandHighBWMessage
			hbbp.BX.High = messageStatusSuccess
			hbbp.DX.High = c.id
			hbbp.SI.SetWord(c.cookie.High.Word())
			hbbp.BP.SetWord(c.cookie.Low.Word())
			hbbp.CX.SetWord(uint32(len(buf)))
			hbbp.DI.SetWord(uint32(uintptr(unsafe.Pointer(&buf[0]))))

			out := hbbp.HighBandwidthIn()
			if (out.BX.High & messageStatusSuccess) == 0 {
				Errorf("Message: Unable to send a message over the communication channel %d", c.id)
				c.reply(messageTypeReceivePayload, messageStatusFail)
				return nil, ErrRpciReceive
			}
		} else {
			b := bytes.NewBuffer(make([]byte, 0, size))

			for {
				if size == 0 {
					break
				}

				bp.CX.High = messageTypeReceivePayload
				bp.BX.Low = messageStatusSuccess

				out = bp.InOut()
				if (out.CX.High & messageStatusSuccess) == 0 {
					if (out.CX.High & messageStatusCheckPoint) != 0 {
						Debugf("A checkpoint occurred. Retrying the operation")
						goto retry
					}

					Errorf("Message: Unable to receive a message over the communication channel %d", c.id)
					c.reply(messageTypeReceivePayload, messageStatusFail)
					return nil, ErrRpciReceive
				}

				if out.DX.High != messageTypeSendPayload {
					Errorf("Message: Protocol error. Expected a MESSAGE_TYPE_SENDPAYLOAD from vmware")
					c.reply(messageTypeReceivePayload, messageStatusFail)
					return nil, ErrRpciReceive
				}

				Debugf("Received %#v", out.BX.Word())

				switch size {
				case 1:
					err = binary.Write(b, binary.LittleEndian, uint8(out.BX.Low))
					size = size - 1

				case 2:
					err = binary.Write(b, binary.LittleEndian, uint16(out.BX.Low))
					size = size - 2

				case 3:
					err = binary.Write(b, binary.LittleEndian, uint16(out.BX.Low))
					if err != nil {
						c.reply(messageTypeReceivePayload, messageStatusFail)
						return nil, ErrRpciReceive
					}
					err = binary.Write(b, binary.LittleEndian, uint8(out.BX.High))
					size = size - 3

				default:
					err = binary.Write(b, binary.LittleEndian, out.BX.Word())
					size = size - 4
				}

				if err != nil {
					Errorf(err.Error())
					c.reply(messageTypeReceivePayload, messageStatusFail)
					return nil, ErrRpciReceive
				}
			}

			buf = b.Bytes()
		}
	}

	c.reply(messageTypeReceiveStatus, messageStatusSuccess)

	return buf, nil
}

func (c *Channel) reply(messageType, messageStatus uint16) {
	bp := &bdoor.BackdoorProto{}

	bp.BX.Low = messageStatus
	bp.CX.High = messageType
	bp.CX.Low = bdoor.CommandMessage
	bp.DX.High = c.id
	bp.SI.SetWord(c.cookie.High.Word())
	bp.DI.SetWord(c.cookie.Low.Word())

	out := bp.InOut()

	/* OUT: Status */
	if (out.CX.High & messageStatusSuccess) == 0 {
		if messageStatus == messageStatusSuccess {
			Errorf("reply Message: Unable to send a message over the communication channel %d", c.id)
		} else {
			Errorf("reply Message: Unable to signal an error of reception over the communication channel %d", c.id)
		}
	}
}

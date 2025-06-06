// Copyright (c) 2021 VMware, Inc. or its affiliates. All Rights Reserved.
// Copyright (c) 2012-2021, Sean Treadway, SoundCloud Ltd.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build integration
// +build integration

package amqp091

import (
	"bytes"
	"context"
	devrand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"net"
	"os"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"go.uber.org/goleak"
)

const envAMQPURLName = "AMQP_URL"

var amqpURL = "amqp://guest:guest@127.0.0.1:5672/"

func init() {
	url := os.Getenv(envAMQPURLName)
	if url != "" {
		amqpURL = url
		return
	}

	fmt.Printf("environment variable envAMQPURLName undefined or empty, using default: %q\n", amqpURL)
}

func TestIntegrationOpenClose(t *testing.T) {
	if c := integrationConnection(t, "open-close"); c != nil {
		t.Logf("have connection, calling connection close")
		if err := c.Close(); err != nil {
			t.Fatalf("connection close: %s", err)
		}
		t.Logf("connection close OK")
	}
}

func TestIntegrationOpenCloseChannel(t *testing.T) {
	if c := integrationConnection(t, "channel"); c != nil {
		defer c.Close()

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("create channel 1: %s", err)
		}
		ch.Close()
	}
}

func TestIntegrationHighChannelChurnInTightLoop(t *testing.T) {
	if c := integrationConnection(t, "channel churn"); c != nil {
		defer c.Close()

		for i := 0; i < 1000; i++ {
			ch, err := c.Channel()
			if err != nil {
				t.Fatalf("create channel 1: %s", err)
			}
			ch.Close()
		}
	}
}

func TestIntegrationOpenConfig(t *testing.T) {
	config := Config{}

	c, err := DialConfig(amqpURL, config)
	if err != nil {
		t.Fatalf("expected to dial with config %+v integration server: %s", config, err)
	}

	if _, err := c.Channel(); err != nil {
		t.Fatalf("expected to open channel: %s", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("expected to close the connection: %s", err)
	}
}

func TestIntegrationOpenConfigWithNetDial(t *testing.T) {
	config := Config{Dial: net.Dial}

	c, err := DialConfig(amqpURL, config)
	if err != nil {
		t.Fatalf("expected to dial with config %+v integration server: %s", config, err)
	}

	if _, err := c.Channel(); err != nil {
		t.Fatalf("expected to open channel: %s", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("expected to close the connection: %s", err)
	}
}

func TestIntegrationLocalAddr(t *testing.T) {
	config := Config{}

	c, err := DialConfig(amqpURL, config)
	if err != nil {
		t.Fatalf("expected to dial with config %+v integration server: %s", config, err)
	}
	defer c.Close()

	a := c.LocalAddr()
	_, portString, err := net.SplitHostPort(a.String())
	if err != nil {
		t.Fatalf("expected to get a local network address with config %+v integration server: %s", config, a.String())
	}

	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatalf("expected to get a TCP port number with config %+v integration server: %s", config, err)
	}
	t.Logf("Connected to port %d\n", port)
}

func TestIntegrationRemoteAddr(t *testing.T) {
	config := Config{}

	c, err := DialConfig(amqpURL, config)
	if err != nil {
		t.Fatalf("expected to dial with config %+v integration server: %s", config, err)
	}
	defer c.Close()

	a := c.RemoteAddr()
	_, portString, err := net.SplitHostPort(a.String())
	if err != nil {
		t.Fatalf("expected to get a remote network address with config %+v integration server: %s", config, a.String())
	}

	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatalf("expected to get a TCP port number with config %+v integration server: %s", config, err)
	}
	t.Logf("Connected to port %d\n", port)
}

// https://github.com/streadway/amqp/issues/94
func TestExchangePassiveOnMissingExchangeShouldError(t *testing.T) {
	c := integrationConnection(t, "exch")
	if c != nil {
		defer c.Close()

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("create channel 1: %s", err)
		}
		defer ch.Close()

		if err := ch.ExchangeDeclarePassive(
			"test-integration-missing-passive-exchange",
			"direct", // type
			false,    // duration (note: is durable)
			true,     // auto-delete
			false,    // internal
			false,    // nowait
			nil,      // args
		); err == nil {
			t.Fatal("ExchangeDeclarePassive of a missing exchange should return error")
		}
	}
}

// https://github.com/streadway/amqp/issues/94
func TestIntegrationExchangeDeclarePassiveOnDeclaredShouldNotError(t *testing.T) {
	c := integrationConnection(t, "exch")
	if c != nil {
		defer c.Close()

		exchange := "test-integration-declared-passive-exchange"

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("create channel: %s", err)
		}
		defer ch.Close()

		if err := ch.ExchangeDeclare(
			exchange, // name
			"direct", // type
			false,    // durable
			true,     // auto-delete
			false,    // internal
			false,    // nowait
			nil,      // args
		); err != nil {
			t.Fatalf("declare exchange: %s", err)
		}

		if err := ch.ExchangeDeclarePassive(
			exchange, // name
			"direct", // type
			false,    // durable
			true,     // auto-delete
			false,    // internal
			false,    // nowait
			nil,      // args
		); err != nil {
			t.Fatalf("ExchangeDeclarePassive on a declared exchange should not error, got: %q", err)
		}
	}
}

func TestIntegrationExchange(t *testing.T) {
	c := integrationConnection(t, "exch")
	if c != nil {
		defer c.Close()

		channel, err := c.Channel()
		if err != nil {
			t.Fatalf("create channel: %s", err)
		}
		t.Logf("create channel OK")

		exchange := "test-integration-exchange"

		if err := channel.ExchangeDeclare(
			exchange, // name
			"direct", // type
			false,    // duration
			true,     // auto-delete
			false,    // internal
			false,    // nowait
			nil,      // args
		); err != nil {
			t.Fatalf("declare exchange: %s", err)
		}
		t.Logf("declare exchange OK")

		if err := channel.ExchangeDelete(exchange, false, false); err != nil {
			t.Fatalf("delete exchange: %s", err)
		}
		t.Logf("delete exchange OK")

		if err := channel.Close(); err != nil {
			t.Fatalf("close channel: %s", err)
		}
		t.Logf("close channel OK")
	}
}

// https://github.com/streadway/amqp/issues/94
func TestIntegrationQueueDeclarePassiveOnMissingExchangeShouldError(t *testing.T) {
	c := integrationConnection(t, "queue")
	if c != nil {
		defer c.Close()

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("create channel1: %s", err)
		}
		defer ch.Close()

		if _, err := ch.QueueDeclarePassive(
			"test-integration-missing-passive-queue", // name
			false,                                    // duration (note: not durable)
			true,                                     // auto-delete
			false,                                    // exclusive
			false,                                    // noWait
			nil,                                      // arguments
		); err == nil {
			t.Fatal("QueueDeclarePassive of a missing queue should error")
		}
	}
}

// https://github.com/rabbitmq/amqp091-go/issues/273
// Note: RabbitMQ behaves according to spec.
// Passive declarations ignore everything but the queue name
// #themoreuknow
func TestIntegrationQueueDeclarePassiveOnQueueTypeMismatchShouldError(t *testing.T) {
	c := integrationConnection(t, t.Name())
	if c != nil {
		defer c.Close()

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("create channel1: %s", err)
		}
		defer ch.Close()

		queueName := t.Name()

		if _, err := ch.QueueDeclare(
			queueName, // name
			false,     // durable
			true,      // auto-delete
			false,     // exclusive
			false,     // noWait
			nil,       // arguments
		); err != nil {
			t.Fatalf("queue declare: %s", err)
		}

		args := Table{QueueTypeArg: QueueTypeQuorum}

		if _, err := ch.QueueDeclarePassive(
			queueName, // name
			false,     // duration (note: not durable)
			true,      // auto-delete
			false,     // exclusive
			false,     // noWait
			args,      // arguments
		); err != nil {
			t.Fatal("QueueDeclarePassive with mismatched queue type should NOT error")
		}
	}
}

// https://github.com/streadway/amqp/issues/94
func TestIntegrationPassiveQueue(t *testing.T) {
	c := integrationConnection(t, "queue")
	if c != nil {
		defer c.Close()

		name := "test-integration-declared-passive-queue"

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("create channel1: %s", err)
		}
		defer ch.Close()

		if _, err := ch.QueueDeclare(
			name,  // name
			false, // durable
			true,  // auto-delete
			false, // exclusive
			false, // noWait
			nil,   // arguments
		); err != nil {
			t.Fatalf("queue declare: %s", err)
		}

		if _, err := ch.QueueDeclarePassive(
			name,  // name
			false, // durable
			true,  // auto-delete
			false, // exclusive
			false, // noWait
			nil,   // arguments
		); err != nil {
			t.Fatalf("QueueDeclarePassive on declared queue should not error, got: %q", err)
		}

		if _, err := ch.QueueDeclarePassive(
			name,  // name
			true,  // durable (note: differs)
			true,  // auto-delete
			false, // exclusive
			false, // noWait
			nil,   // arguments
		); err != nil {
			t.Fatalf("QueueDeclarePassive on declared queue with different flags should error")
		}
	}
}

func TestIntegrationBasicQueueOperations(t *testing.T) {
	c := integrationConnection(t, "queue")
	if c != nil {
		defer c.Close()

		channel, err := c.Channel()
		if err != nil {
			t.Fatalf("create channel: %s", err)
		}
		t.Logf("create channel OK")

		exchangeName := "test-basic-ops-exchange"
		queueName := "test-basic-ops-queue"

		deleteQueueFirstOptions := []bool{true, false}
		for _, deleteQueueFirst := range deleteQueueFirstOptions {
			if err := channel.ExchangeDeclare(
				exchangeName, // name
				"direct",     // type
				true,         // duration (note: is durable)
				false,        // auto-delete
				false,        // internal
				false,        // nowait
				nil,          // args
			); err != nil {
				t.Fatalf("declare exchange: %s", err)
			}
			t.Logf("declare exchange OK")

			if _, err := channel.QueueDeclare(
				queueName, // name
				true,      // duration (note: durable)
				false,     // auto-delete
				false,     // exclusive
				false,     // noWait
				nil,       // arguments
			); err != nil {
				t.Fatalf("queue declare: %s", err)
			}
			t.Logf("declare queue OK")

			if err := channel.QueueBind(
				queueName,    // name
				"",           // routingKey
				exchangeName, // sourceExchange
				false,        // noWait
				nil,          // arguments
			); err != nil {
				t.Fatalf("queue bind: %s", err)
			}
			t.Logf("queue bind OK")

			if deleteQueueFirst {
				if _, err := channel.QueueDelete(
					queueName, // name
					false,     // ifUnused (false=be aggressive)
					false,     // ifEmpty (false=be aggressive)
					false,     // noWait
				); err != nil {
					t.Fatalf("delete queue (first): %s", err)
				}
				t.Logf("delete queue (first) OK")

				if err := channel.ExchangeDelete(exchangeName, false, false); err != nil {
					t.Fatalf("delete exchange (after delete queue): %s", err)
				}
				t.Logf("delete exchange (after delete queue) OK")
			} else { // deleteExchangeFirst
				if err := channel.ExchangeDelete(exchangeName, false, false); err != nil {
					t.Fatalf("delete exchange (first): %s", err)
				}
				t.Logf("delete exchange (first) OK")

				if _, err := channel.QueueDeclarePassive(
					queueName,
					true,
					false,
					false,
					false,
					nil,
				); err != nil {
					t.Fatalf("inspect queue state after deleting exchange: %s", err)
				}
				t.Logf("queue properly remains after exchange is deleted")

				if _, err := channel.QueueDelete(
					queueName,
					false, // ifUnused
					false, // ifEmpty
					false, // noWait
				); err != nil {
					t.Fatalf("delete queue (after delete exchange): %s", err)
				}
				t.Logf("delete queue (after delete exchange) OK")
			}
		}

		if err := channel.Close(); err != nil {
			t.Fatalf("close channel: %s", err)
		}
		t.Logf("close channel OK")
	}
}

func TestIntegrationConnectionNegotiatesMaxChannels(t *testing.T) {
	config := Config{ChannelMax: 0}

	c, err := DialConfig(amqpURL, config)
	if err != nil {
		t.Fatalf("expected to dial with config %+v integration server: %s", config, err)
	}
	defer c.Close()

	if want, got := defaultChannelMax, c.Config.ChannelMax; want != got {
		t.Fatalf("expected connection to negotiate uint16 (%d) channels, got: %d", want, got)
	}
}

func TestIntegrationConnectionNegotiatesClientMaxChannels(t *testing.T) {
	config := Config{ChannelMax: 16}

	c, err := DialConfig(amqpURL, config)
	if err != nil {
		t.Fatalf("expected to dial with config %+v integration server: %s", config, err)
	}
	defer c.Close()

	if want, got := config.ChannelMax, c.Config.ChannelMax; want != got {
		t.Fatalf("expected client specified channel limit after handshake %d, got: %d", want, got)
	}
}

func TestIntegrationChannelIDsExhausted(t *testing.T) {
	config := Config{ChannelMax: 16}

	c, err := DialConfig(amqpURL, config)
	if err != nil {
		t.Fatalf("expected to dial with config %+v integration server: %s", config, err)
	}
	defer c.Close()

	for i := uint16(1); i <= c.Config.ChannelMax; i++ {
		if _, err := c.Channel(); err != nil {
			t.Fatalf("expected allocating all channel ids to succed, failed on %d with %v", i, err)
		}
	}

	if _, err := c.Channel(); err != ErrChannelMax {
		t.Fatalf("expected allocating all channels to produce the client side error %#v, got: %#v", ErrChannelMax, err)
	}
}

func TestIntegrationChannelClosing(t *testing.T) {
	c := integrationConnection(t, "closings")
	if c != nil {
		defer c.Close()

		// open and close
		channel, err := c.Channel()
		if err != nil {
			t.Fatalf("basic create channel: %s", err)
		}
		t.Logf("basic create channel OK")

		if err := channel.Close(); err != nil {
			t.Fatalf("basic close channel: %s", err)
		}
		t.Logf("basic close channel OK")

		// deferred close
		signal := make(chan bool)
		err0 := make(chan error)
		err1 := make(chan error)

		go func() {
			channel, err := c.Channel()
			if err != nil {
				err0 <- err
			}

			<-signal // a bit of synchronization

			defer func() {
				if err := channel.Close(); err != nil {
					err1 <- err
				}
				signal <- true
			}()
		}()

		signal <- true

		select {
		case e0 := <-err0:
			t.Fatalf("second create channel: %s", e0)
		case e1 := <-err1:
			t.Fatalf("deferred close channel: %s", e1)
		case <-signal:
			t.Logf("(got close signal OK)")
			break
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("deferred close: timeout")
		}

		// multiple channels
		for _, n := range []int{2, 4, 8, 16, 32, 64, 128, 256} {
			channels := make([]*Channel, n)
			for i := 0; i < n; i++ {
				var err error
				if channels[i], err = c.Channel(); err != nil {
					t.Fatalf("create channel %d/%d: %s", i+1, n, err)
				}
			}
			for i, channel := range channels {
				if err := channel.Close(); err != nil {
					t.Fatalf("close channel %d/%d: %s", i+1, n, err)
				}
			}
			t.Logf("created/closed %d channels OK", n)
		}
	}
}

func TestIntegrationMeaningfulChannelErrors(t *testing.T) {
	c := integrationConnection(t, "pub")
	if c != nil {
		defer c.Close()

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("Could not create channel")
		}

		queue := "test.integration.channel.error"

		_, err = ch.QueueDeclare(queue, false, true, false, false, nil)
		if err != nil {
			t.Fatalf("Could not declare")
		}

		_, err = ch.QueueDeclare(queue, true, false, false, false, nil)
		if err == nil {
			t.Fatalf("Expected error, got nil")
		}

		e, ok := err.(*Error)
		if !ok {
			t.Fatalf("Expected type Error response, got %T", err)
		}

		if e.Code != PreconditionFailed {
			t.Fatalf("Expected PreconditionFailed, got: %+v", e)
		}

		_, err = ch.QueueDeclare(queue, false, true, false, false, nil)
		if err != ErrClosed {
			t.Fatalf("Expected channel to be closed, got: %T", err)
		}
	}
}

// https://github.com/streadway/amqp/issues/6
func TestIntegrationNonBlockingClose(t *testing.T) {
	c := integrationConnection(t, "#6")
	if c != nil {
		defer c.Close()

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("Could not create channel")
		}

		queue := "test.integration.blocking.close"

		_, err = ch.QueueDeclare(queue, false, true, false, false, nil)
		if err != nil {
			t.Fatalf("Could not declare")
		}

		msgs, err := ch.Consume(queue, "", false, false, false, false, nil)
		if err != nil {
			t.Fatalf("Could not consume")
		}

		// Simulate a consumer
		go func() {
			for range msgs {
				t.Logf("Oh my, received message on an empty queue")
			}
		}()

		succeed := make(chan bool)
		errs := make(chan error)

		go func() {
			if err = ch.Close(); err != nil {
				errs <- err
			} else {
				succeed <- true
			}
		}()

		select {
		case <-errs:
			t.Fatalf("Close produced an error when it shouldn't")
		case <-succeed:
			break
		case <-time.After(1 * time.Second):
			t.Fatalf("Close timed out after 1s")
		}
	}
}

func TestIntegrationPublishConsume(t *testing.T) {
	queue := "test.integration.publish.consume"

	c1 := integrationConnection(t, "pub")
	c2 := integrationConnection(t, "sub")

	if c1 != nil && c2 != nil {
		defer c1.Close()
		defer c2.Close()

		pub, _ := c1.Channel()
		sub, _ := c2.Channel()

		if _, e := pub.QueueDeclare(queue, false, true, false, false, nil); e != nil {
			t.Fatalf("error declaring queue %s: %v", queue, e)
		}

		if _, e := sub.QueueDeclare(queue, false, true, false, false, nil); e != nil {
			t.Fatalf("error declaring queue %s: %v", queue, e)
		}

		defer integrationQueueDelete(t, pub, queue)

		messages, _ := sub.Consume(queue, "", false, false, false, false, nil)

		if e := pub.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("pub 1")}); e != nil {
			t.Fatalf("publish error: %v", e)
		}
		if e := pub.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("pub 2")}); e != nil {
			t.Fatalf("publish error: %v", e)
		}
		if e := pub.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("pub 3")}); e != nil {
			t.Fatalf("publish error: %v", e)
		}

		assertConsumeBody(t, messages, []byte("pub 1"))
		assertConsumeBody(t, messages, []byte("pub 2"))
		assertConsumeBody(t, messages, []byte("pub 3"))
	}
}

func TestIntegrationConsumeFlow(t *testing.T) {
	queue := "test.integration.consumer-flow"

	c1 := integrationConnection(t, "pub-flow")
	c2 := integrationConnection(t, "sub-flow")

	if c1 != nil && c2 != nil {
		defer c1.Close()
		defer c2.Close()

		pub, _ := c1.Channel()
		sub, _ := c2.Channel()

		if _, e := pub.QueueDeclare(queue, false, true, false, false, nil); e != nil {
			t.Fatalf("error declaring queue %s: %v", queue, e)
		}

		if _, e := sub.QueueDeclare(queue, false, true, false, false, nil); e != nil {
			t.Fatalf("error declaring queue %s: %v", queue, e)
		}

		defer integrationQueueDelete(t, pub, queue)

		if err := sub.Qos(1, 0, false); err != nil {
			t.Fatalf("error setting QoS: %v", err)
		}

		var messages <-chan Delivery
		var err error
		if messages, err = sub.Consume(queue, "", false, false, false, false, nil); err != nil {
			t.Fatalf("error consuming: %v", err)
		}

		if e := pub.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("pub 1")}); e != nil {
			t.Fatalf("error publishing: %v", e)
		}
		if e := pub.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("pub 2")}); e != nil {
			t.Fatalf("error publishing: %v", e)
		}

		msg := assertConsumeBody(t, messages, []byte("pub 1"))

		if err := sub.Flow(false); err.(*Error).Code == NotImplemented {
			t.Log("flow control is not supported on this version of rabbitmq")
			return
		}

		if e := msg.Ack(false); e != nil {
			t.Fatalf("error acking: %v", e)
		}

		select {
		case <-messages:
			t.Fatalf("message was delivered when flow was not active")
		default:
		}

		if e := sub.Flow(true); e != nil {
			t.Fatalf("error flow: %v", e)
		}

		msg = assertConsumeBody(t, messages, []byte("pub 2"))
		if e := msg.Ack(false); e != nil {
			t.Fatalf("error acking: %v", e)
		}
	}
}

func TestIntegrationRecoverNotImplemented(t *testing.T) {
	// TODO: remove this when Channel.Recover is removed
	queue := "test.recover"

	if c, ch := integrationQueue(t, queue); c != nil {
		if product, ok := c.Properties["product"]; ok && product.(string) == "RabbitMQ" {
			defer c.Close()

			err := ch.Recover(false)

			if ex, ok := err.(*Error); !ok || ex.Code != 540 {
				t.Fatalf("Expected NOT IMPLEMENTED got: %v", ex)
			}
		}
	}
}

// This test is driven by a private API to simulate the server sending a channelFlow message
func TestIntegrationPublishFlow(t *testing.T) {
	// TODO - no idea how to test without affecting the server or mucking internal APIs
	// i'd like to make sure the RW lock can be held by multiple publisher threads
	// and that multiple channelFlow messages do not block the dispatch thread
}

func TestIntegrationConsumeCancel(t *testing.T) {
	queue := "test.integration.consume-cancel"

	c := integrationConnection(t, "pub")

	if c != nil {
		defer c.Close()

		ch, _ := c.Channel()

		if _, e := ch.QueueDeclare(queue, false, true, false, false, nil); e != nil {
			t.Fatalf("error declaring queue %s: %v", queue, e)
		}

		defer integrationQueueDelete(t, ch, queue)

		messages, _ := ch.Consume(queue, "integration-tag", false, false, false, false, nil)

		if e := ch.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("1")}); e != nil {
			t.Fatalf("error publishing: %v", e)
		}

		assertConsumeBody(t, messages, []byte("1"))

		err := ch.Cancel("integration-tag", false)
		if err != nil {
			t.Fatalf("error cancelling the consumer: %v", err)
		}

		if e := ch.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("2")}); e != nil {
			t.Fatalf("error publishing: %v", e)
		}

		select {
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("Timeout on Close")
		case _, ok := <-messages:
			if ok {
				t.Fatalf("Extra message on consumer when consumer should have been closed")
			}
		}
	}
}

func TestIntegrationConsumeCancelWithContext(t *testing.T) {
	queue := "test.integration.consume-cancel-with-context"

	c := integrationConnection(t, "pub")

	if c != nil {
		defer c.Close()

		ch, _ := c.Channel()

		if _, e := ch.QueueDeclare(queue, false, true, false, false, nil); e != nil {
			t.Fatalf("error declaring queue %s: %v", queue, e)
		}

		defer integrationQueueDelete(t, ch, queue)

		ctx, cancel := context.WithCancel(context.Background())
		messages, _ := ch.ConsumeWithContext(ctx, queue, "integration-tag-with-context", false, false, false, false, nil)

		if e := ch.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("1")}); e != nil {
			t.Fatalf("error publishing: %v", e)
		}

		assertConsumeBody(t, messages, []byte("1"))

		cancel()
		<-time.After(200 * time.Millisecond) // wait to call cancel asynchronously

		if e := ch.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("2")}); e != nil {
			t.Fatalf("error publishing: %v", e)
		}

		select {
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("Timeout on Close")
		case _, ok := <-messages:
			if ok {
				t.Fatalf("Extra message on consumer when consumer should have been closed")
			}
		}
	}
}

func (c *Connection) Generate(_ *rand.Rand, _ int) reflect.Value {
	urlStr := amqpURL

	conn, err := Dial(urlStr)
	if err != nil {
		return reflect.ValueOf(nil)
	}

	return reflect.ValueOf(conn)
}

func (c Publishing) Generate(r *rand.Rand, _ int) reflect.Value {
	var ok bool
	var t reflect.Value

	p := Publishing{}
	// p.DeliveryMode = uint8(r.Intn(3))
	// p.Priority = uint8(r.Intn(8))

	if r.Intn(2) > 0 {
		p.ContentType = "application/octet-stream"
	}

	if r.Intn(2) > 0 {
		p.ContentEncoding = "gzip"
	}

	if r.Intn(2) > 0 {
		p.CorrelationId = strconv.Itoa(r.Int())
	}

	if r.Intn(2) > 0 {
		p.ReplyTo = strconv.Itoa(r.Int())
	}

	if r.Intn(2) > 0 {
		p.MessageId = strconv.Itoa(r.Int())
	}

	if r.Intn(2) > 0 {
		p.Type = strconv.Itoa(r.Int())
	}

	if r.Intn(2) > 0 {
		p.AppId = strconv.Itoa(r.Int())
	}

	if r.Intn(2) > 0 {
		p.Timestamp = time.Unix(r.Int63(), r.Int63())
	}

	if t, ok = quick.Value(reflect.TypeOf(p.Body), r); ok {
		p.Body = t.Bytes()
	}

	return reflect.ValueOf(p)
}

func TestQuickPublishOnly(t *testing.T) {
	if c := integrationConnection(t, "quick"); c != nil {
		defer c.Close()
		pub, err := c.Channel()
		if err != nil {
			t.Fatalf("getting channel failed: %s", err)
		}
		queue := "test-publish"

		if _, err = pub.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Failed to declare: %s", err)
			return
		}

		defer integrationQueueDelete(t, pub, queue)

		chk := func(msg Publishing) bool {
			return pub.PublishWithContext(context.TODO(), "", queue, false, false, msg) == nil
		}
		if err := quick.Check(chk, nil); err != nil {
			t.Fatalf("check error: %v", err)
		}
	}
}

func TestPublishEmptyBody(t *testing.T) {
	c := integrationConnection(t, "empty")
	if c != nil {
		defer c.Close()

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("Failed to create channel")
		}

		queue := "test-TestPublishEmptyBody"

		if _, err := ch.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Could not declare")
		}

		defer integrationQueueDelete(t, ch, queue)

		messages, err := ch.Consume(queue, "", false, false, false, false, nil)
		if err != nil {
			t.Fatalf("Could not consume")
		}

		err = ch.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{})
		if err != nil {
			t.Fatalf("Could not publish")
		}

		select {
		case msg := <-messages:
			if len(msg.Body) != 0 {
				t.Fatalf("Received non empty body")
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("Timeout on receive")
		}
	}
}

func TestPublishEmptyBodyWithHeadersIssue67(t *testing.T) {
	c := integrationConnection(t, "issue67")
	if c != nil {
		defer c.Close()

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("Failed to create channel")
			return
		}

		queue := "test-TestPublishEmptyBodyWithHeaders"

		if _, err := ch.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Could not declare")
		}

		defer integrationQueueDelete(t, ch, queue)

		messages, err := ch.Consume(queue, "", false, false, false, false, nil)
		if err != nil {
			t.Fatalf("Could not consume")
		}

		headers := Table{
			"ham": "spam",
		}

		err = ch.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Headers: headers})
		if err != nil {
			t.Fatalf("Could not publish")
		}

		select {
		case msg := <-messages:
			if msg.Headers["ham"] == nil {
				t.Fatalf("Headers aren't sent")
			}
			if msg.Headers["ham"] != "spam" {
				t.Fatalf("Headers are wrong")
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("Timeout on receive")
		}
	}
}

func TestQuickPublishConsumeOnly(t *testing.T) {
	c1 := integrationConnection(t, "quick-pub")
	c2 := integrationConnection(t, "quick-sub")

	if c1 != nil && c2 != nil {
		defer c1.Close()
		defer c2.Close()

		pub, err := c1.Channel()
		if err != nil {
			t.Fatalf("getting channel1 failed: %s", err)
		}

		sub, err := c2.Channel()
		if err != nil {
			t.Fatalf("getting channel2 failed: %s", err)
		}

		queue := "TestPublishConsumeOnly"

		if _, err = pub.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Failed to declare: %s", err)
			return
		}

		if _, err = sub.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Failed to declare: %s", err)
			return
		}

		defer integrationQueueDelete(t, sub, queue)

		ch, err := sub.Consume(queue, "", false, false, false, false, nil)
		if err != nil {
			t.Fatalf("Could not sub: %s", err)
		}

		if chkerr := quick.CheckEqual(
			func(msg Publishing) []byte {
				empty := Publishing{Body: msg.Body}
				if pub.PublishWithContext(context.TODO(), "", queue, false, false, empty) != nil {
					return []byte{'X'}
				}
				return msg.Body
			},
			func(_ Publishing) []byte {
				out := <-ch
				if out.Ack(false) != nil {
					return []byte{'X'}
				}
				return out.Body
			},
			nil); chkerr != nil {
			t.Fatalf("check error: %v", chkerr)
		}
	}
}

func TestQuickPublishConsumeBigBody(t *testing.T) {
	c1 := integrationConnection(t, "big-pub")
	c2 := integrationConnection(t, "big-sub")

	if c1 != nil && c2 != nil {
		defer c1.Close()
		defer c2.Close()

		pub, err := c1.Channel()
		if err != nil {
			t.Fatalf("getting channel 1 failed: %s", err)
		}
		sub, err := c2.Channel()
		if err != nil {
			t.Fatalf("getting channel 2 failed: %s", err)
		}

		queue := "test-pubsub"

		if _, err = sub.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Failed to declare: %s", err)
		}

		ch, err := sub.Consume(queue, "", false, false, false, false, nil)
		if err != nil {
			t.Fatalf("Could not sub: %s", err)
		}

		fixture := Publishing{
			Body: make([]byte, 1e4+1000),
		}

		if _, err = pub.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Failed to declare: %s", err)
		}

		err = pub.PublishWithContext(context.TODO(), "", queue, false, false, fixture)
		if err != nil {
			t.Fatalf("Could not publish big body")
		}

		select {
		case msg := <-ch:
			if !bytes.Equal(msg.Body, fixture.Body) {
				t.Fatalf("Consumed big body didn't match")
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("Timeout on receive")
		}
	}
}

func TestIntegrationGetOk(t *testing.T) {
	if c := integrationConnection(t, "getok"); c != nil {
		defer c.Close()

		queue := "test.get-ok"
		ch, _ := c.Channel()

		if _, err := ch.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Failed to declare: %s", err)
		}

		if err := ch.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("ok")}); err != nil {
			t.Fatalf("Failed to publish: %s", err)
		}

		msg, ok, err := ch.Get(queue, false)
		if err != nil {
			t.Fatalf("Failed get: %v", err)
		}

		if !ok {
			t.Fatalf("Get on a queued message did not find the message")
		}

		if string(msg.Body) != "ok" {
			t.Fatalf("Get did not get the correct message")
		}
	}
}

func TestIntegrationGetEmpty(t *testing.T) {
	if c := integrationConnection(t, "getok"); c != nil {
		defer c.Close()

		queue := "test.get-ok"
		ch, _ := c.Channel()

		if _, err := ch.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Failed to declare: %s", err)
		}

		_, ok, err := ch.Get(queue, false)
		if err != nil {
			t.Fatalf("Failed get: %v", err)
		}

		if !ok {
			t.Fatalf("Get on a queued message retrieved a message when it shouldn't have")
		}
	}
}

func TestIntegrationTxCommit(t *testing.T) {
	if c := integrationConnection(t, "txcommit"); c != nil {
		defer c.Close()

		queue := "test.tx.commit"
		ch, _ := c.Channel()

		if _, err := ch.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Failed to declare: %s", err)
		}

		if err := ch.Tx(); err != nil {
			t.Fatalf("tx.select failed")
		}

		if err := ch.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("ok")}); err != nil {
			t.Fatalf("Failed to publish: %s", err)
		}

		if err := ch.TxCommit(); err != nil {
			t.Fatalf("tx.commit failed")
		}

		msg, ok, err := ch.Get(queue, false)

		if err != nil || !ok {
			t.Fatalf("Failed get: %v", err)
		}

		if string(msg.Body) != "ok" {
			t.Fatalf("Get did not get the correct message from the transaction")
		}
	}
}

func TestIntegrationTxRollback(t *testing.T) {
	if c := integrationConnection(t, "txrollback"); c != nil {
		defer c.Close()

		queue := "test.tx.rollback"
		ch, _ := c.Channel()

		if _, err := ch.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Failed to declare: %s", err)
		}

		if err := ch.Tx(); err != nil {
			t.Fatalf("tx.select failed")
		}

		if err := ch.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("ok")}); err != nil {
			t.Fatalf("Failed to publish: %s", err)
		}

		if err := ch.TxRollback(); err != nil {
			t.Fatalf("tx.rollback failed")
		}

		_, ok, err := ch.Get(queue, false)
		if err != nil {
			t.Fatalf("Failed get: %v", err)
		}

		if ok {
			t.Fatalf("message was published when it should have been rolled back")
		}
	}
}

func TestIntegrationReturn(t *testing.T) {
	if c, ch := integrationQueue(t, "return"); c != nil {
		defer c.Close()

		ret := make(chan Return, 1)

		ch.NotifyReturn(ret)

		// mandatory publish to an exchange without a binding should be returned
		if err := ch.PublishWithContext(context.TODO(), "", "return-without-binding", true, false, Publishing{Body: []byte("mandatory")}); err != nil {
			t.Fatalf("Failed to publish: %s", err)
		}

		select {
		case res := <-ret:
			if string(res.Body) != "mandatory" {
				t.Fatalf("expected return of the same message")
			}

			if res.ReplyCode != NoRoute {
				t.Fatalf("expected no consumers reply code on the Return result, got: %v", res.ReplyCode)
			}

		case <-time.After(200 * time.Millisecond):
			t.Fatalf("no return was received within 200ms")
		}
	}
}

func TestIntegrationCancel(t *testing.T) {
	queue := "cancel"
	consumerTag := "test.cancel"

	if c, ch := integrationQueue(t, queue); c != nil {
		defer c.Close()

		cancels := ch.NotifyCancel(make(chan string, 1))
		consumeErr := make(chan error, 1)
		deleteErr := make(chan error, 1)

		go func() {
			if _, err := ch.Consume(queue, consumerTag, false, false, false, false, nil); err != nil {
				consumeErr <- err
			}
			if _, err := ch.QueueDelete(queue, false, false, false); err != nil {
				deleteErr <- err
			}
		}()

		select {
		case err := <-consumeErr:
			t.Fatalf("cannot consume from %q to test NotifyCancel: %v", queue, err)
		case err := <-deleteErr:
			t.Fatalf("cannot delete integration queue: %v", err)
		case tag := <-cancels:
			if want, got := consumerTag, tag; want != got {
				t.Fatalf("expected to be notified of deleted queue with consumer tag, got: %q", got)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("expected to be notified of deleted queue with 200ms")
		}
	}
}

func TestIntegrationConfirm(t *testing.T) {
	if c, ch := integrationQueue(t, "confirm"); c != nil {
		defer c.Close()

		confirms := ch.NotifyPublish(make(chan Confirmation, 1))

		if err := ch.Confirm(false); err != nil {
			t.Fatalf("could not confirm")
		}

		if err := ch.PublishWithContext(context.TODO(), "", "confirm", false, false, Publishing{Body: []byte("confirm")}); err != nil {
			t.Fatalf("Failed to publish: %s", err)
		}

		select {
		case confirmed := <-confirms:
			if confirmed.DeliveryTag != 1 {
				t.Fatalf("expected ack starting with delivery tag of 1")
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("no ack was received within 200ms")
		}
	}
}

// https://github.com/streadway/amqp/issues/61
func TestRoundTripAllFieldValueTypes61(t *testing.T) {
	if conn := integrationConnection(t, "issue61"); conn != nil {
		defer conn.Close()
		timestamp := time.Unix(100000000, 0)

		headers := Table{
			"A": []interface{}{
				[]interface{}{"nested array", int32(3)},
				Decimal{2, 1},
				Table{"S": "nested table in array"},
				int32(2 << 20),
				string("array string"),
				timestamp,
				nil,
				byte(2),
				int8(-2),
				float64(2.64),
				float32(2.32),
				int64(2 << 60),
				int16(2 << 10),
				bool(true),
				[]byte{'b', '2'},
			},
			"D": Decimal{1, 1},
			"F": Table{"S": "nested table in table"},
			"I": int32(1 << 20),
			"S": string("string"),
			"T": timestamp,
			"V": nil,
			"B": byte(1),
			"b": int8(-1),
			"d": float64(1.64),
			"f": float32(1.32),
			"l": int64(1 << 60),
			"s": int16(1 << 10),
			"t": bool(true),
			"x": []byte{'b', '1'},
		}

		queue := "test.issue61-roundtrip"
		ch, _ := conn.Channel()

		if _, err := ch.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Could not declare")
		}

		msgs, err := ch.Consume(queue, "", false, false, false, false, nil)
		if err != nil {
			t.Fatalf("Could not consume")
		}

		err = ch.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("ignored"), Headers: headers})
		if err != nil {
			t.Fatalf("Could not publish: %v", err)
		}

		msg, ok := <-msgs

		if !ok {
			t.Fatalf("Channel closed prematurely likely due to publish exception")
		}

		for k, v := range headers {
			if !reflect.DeepEqual(v, msg.Headers[k]) {
				t.Fatalf("Round trip header not the same for key %q: expected: %#v, got %#v", k, v, msg.Headers[k])
			}
		}
	}
}

// Declares a queue with the x-message-ttl extension to exercise integer
// serialization.
//
// Relates to https://github.com/streadway/amqp/issues/60
func TestDeclareArgsXMessageTTL(t *testing.T) {
	if conn := integrationConnection(t, "declareTTL"); conn != nil {
		defer conn.Close()

		ch, _ := conn.Channel()
		args := Table{"x-message-ttl": int32(9000000)}

		// should not drop the connection
		if _, err := ch.QueueDeclare("declareWithTTL", false, true, false, false, args); err != nil {
			t.Fatalf("cannot declare with TTL: got: %v", err)
		}
	}
}

// Sets up the topology where rejected messages will be forwarded
// to a fanout exchange, with a single queue bound.
//
// Relates to https://github.com/streadway/amqp/issues/56
func TestDeclareArgsRejectToDeadLetterQueue(t *testing.T) {
	if conn := integrationConnection(t, "declareArgs"); conn != nil {
		defer conn.Close()

		ex, q := "declareArgs", "declareArgs-deliveries"
		dlex, dlq := ex+"-dead-letter", q+"-dead-letter"

		ch, _ := conn.Channel()

		if err := ch.ExchangeDeclare(ex, "fanout", false, true, false, false, nil); err != nil {
			t.Fatalf("cannot declare %v: got: %v", ex, err)
		}

		if err := ch.ExchangeDeclare(dlex, "fanout", false, true, false, false, nil); err != nil {
			t.Fatalf("cannot declare %v: got: %v", dlex, err)
		}

		if _, err := ch.QueueDeclare(dlq, false, true, false, false, nil); err != nil {
			t.Fatalf("cannot declare %v: got: %v", dlq, err)
		}

		if err := ch.QueueBind(dlq, "#", dlex, false, nil); err != nil {
			t.Fatalf("cannot bind %v to %v: got: %v", dlq, dlex, err)
		}

		if _, err := ch.QueueDeclare(q, false, true, false, false, Table{
			"x-dead-letter-exchange": dlex,
		}); err != nil {
			t.Fatalf("cannot declare %v with dlq %v: got: %v", q, dlex, err)
		}

		if err := ch.QueueBind(q, "#", ex, false, nil); err != nil {
			t.Fatalf("cannot bind %v: got: %v", ex, err)
		}

		fails, err := ch.Consume(q, "", false, false, false, false, nil)
		if err != nil {
			t.Fatalf("cannot consume %v: got: %v", q, err)
		}

		// Reject everything consumed
		rejectErrs := make(chan error, len(fails))
		go func() {
			for d := range fails {
				if err := d.Reject(false); err != nil {
					rejectErrs <- err
				}
			}
		}()

		// Publish the 'poison'
		if err := ch.PublishWithContext(context.TODO(), ex, q, true, false, Publishing{Body: []byte("ignored")}); err != nil {
			t.Fatalf("publishing failed")
		}

		// spin-get until message arrives at the dead-letter queue with a
		// synchronous parse to exercise the array field (x-death) set by the
		// server relating to issue-56
		for i := 0; i < 10; i++ {
			d, got, err := ch.Get(dlq, false)
			if !got && err == nil {
				continue
			} else if err != nil {
				t.Fatalf("expected success in parsing reject, got: %v", err)
			} else {
				// pass if we've parsed an array
				if v, ok := d.Headers["x-death"]; ok {
					if _, ok := v.([]interface{}); ok {
						return
					}
				}
				t.Fatalf("array field x-death expected in the headers, got: %v (%T)", d.Headers, d.Headers["x-death"])
			}
		}
		t.Fatalf("expected dead-letter after 10 get attempts")
	}
}

// https://github.com/streadway/amqp/issues/48
func TestDeadlockConsumerIssue48(t *testing.T) {
	if conn := integrationConnection(t, "issue48"); conn != nil {
		defer conn.Close()

		deadline := make(chan bool)
		go func() {
			select {
			case <-time.After(5 * time.Second):
				panic("expected to receive 2 deliveries while in an RPC, got a deadlock")
			case <-deadline:
				// pass
			}
		}()

		ch, err := conn.Channel()
		if err != nil {
			t.Fatalf("got error on channel.open: %v", err)
		}

		queue := "test-issue48"

		if _, err := ch.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("expected to declare a queue: %v", err)
		}

		if err := ch.Confirm(false); err != nil {
			t.Fatalf("got error on confirm: %v", err)
		}

		confirms := ch.NotifyPublish(make(chan Confirmation, 2))

		for i := 0; i < cap(confirms); i++ {
			// Fill the queue with some new or remaining publishings
			if err := ch.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("")}); err != nil {
				t.Fatalf("error publishing: %v", err)
			}
		}

		for i := 0; i < cap(confirms); i++ {
			// Wait for them to land on the queue, so they'll be delivered on consume
			<-confirms
		}

		// Consuming should send them all on the wire
		msgs, err := ch.Consume(queue, "", false, false, false, false, nil)
		if err != nil {
			t.Fatalf("got error on consume: %v", err)
		}

		// We pop one off the chan, the other is on the wire
		<-msgs

		// Opening a new channel (any RPC) while another delivery is on the wire
		if _, err := conn.Channel(); err != nil {
			t.Fatalf("got error on consume: %v", err)
		}

		// We pop the next off the chan
		<-msgs

		deadline <- true
	}
}

// https://github.com/streadway/amqp/issues/46
func TestRepeatedChannelExceptionWithPublishAndMaxProcsIssue46(t *testing.T) {
	conn := integrationConnection(t, "issue46")
	if conn == nil {
		t.Fatal("conn is nil")
	}

	t.Cleanup(func() {
		conn.Close()
	})

	for i := 0; i < 100; i++ {
		if conn.IsClosed() {
			t.Fatal("conn is closed")
		}

		ch, channelOpenError := conn.Channel()
		if channelOpenError != nil {
			t.Fatalf("error opening channel: %d error: %+v", i, channelOpenError)
		}

		for j := 0; j < 100; j++ {
			if ch.IsClosed() {
				if j == 0 {
					t.Fatal("channel should not be closed")
				}
				break
			}
			err := ch.PublishWithContext(context.TODO(), "not-existing-exchange", "some-key", false, false, Publishing{Body: []byte("some-data")})
			if err != nil {
				if publishError, ok := err.(*Error); !ok || publishError.Code != 504 {
					t.Fatalf("expected channel only exception i: %d j: %d error: %+v", i, j, publishError)
				}
			}
		}
	}
}

// https://github.com/streadway/amqp/issues/43
func TestChannelExceptionWithCloseIssue43(t *testing.T) {
	conn := integrationConnection(t, "issue43")
	if conn != nil {
		t.Cleanup(func() { conn.Close() })

		go func() {
			for err := range conn.NotifyClose(make(chan *Error)) {
				t.Log(err.Error())
			}
		}()

		c1, err := conn.Channel()
		if err != nil {
			t.Fatalf("failed to create channel, got: %v", err)
		}

		go func() {
			for err := range c1.NotifyClose(make(chan *Error)) {
				t.Log("Channel1 Close: " + err.Error())
			}
		}()

		c2, err := conn.Channel()
		if err != nil {
			t.Fatalf("failed to create channel, got: %v", err)
		}

		go func() {
			for err := range c2.NotifyClose(make(chan *Error)) {
				t.Log("Channel2 Close: " + err.Error())
			}
		}()

		// Cause an asynchronous channel exception causing the server
		// to send a "channel.close" method either before or after the next
		// asynchronous method.
		err = c1.PublishWithContext(context.TODO(), "nonexisting-exchange", "", false, false, Publishing{})
		if err != nil {
			t.Fatalf("failed to publish, got: %v", err)
		}

		// Receive or send the channel close method, the channel shuts down
		// but this expects a channel.close-ok to be received.
		c1.Close()

		// This ensures that the 2nd channel is unaffected by the channel exception
		// on channel 1.
		err = c2.ExchangeDeclare("test-channel-still-exists", "direct", false, true, false, false, nil)
		if err != nil {
			t.Fatalf("failed to declare exchange, got: %v", err)
		}
	}
}

// https://github.com/streadway/amqp/issues/7
func TestCorruptedMessageIssue7(t *testing.T) {
	messageCount := 1024

	c1 := integrationConnection(t, "")
	c2 := integrationConnection(t, "")

	if c1 != nil && c2 != nil {
		defer c1.Close()
		defer c2.Close()

		pub, err := c1.Channel()
		if err != nil {
			t.Fatalf("Cannot create Channel")
		}

		sub, err := c2.Channel()
		if err != nil {
			t.Fatalf("Cannot create Channel")
		}

		queue := "test-corrupted-message-regression"

		if _, err := pub.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Cannot declare")
		}

		if _, err := sub.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("Cannot declare")
		}

		msgs, err := sub.Consume(queue, "", false, false, false, false, nil)
		if err != nil {
			t.Fatalf("Cannot consume")
		}

		for i := 0; i < messageCount; i++ {
			err := pub.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{
				Body: generateCrc32Random(t, 7*i),
			})
			if err != nil {
				t.Fatalf("Failed to publish")
			}
		}

		for i := 0; i < messageCount; i++ {
			select {
			case msg := <-msgs:
				assertMessageCrc32(t, msg.Body, fmt.Sprintf("missed match at %d", i))
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("Timeout on recv")
			}
		}
	}
}

// https://github.com/streadway/amqp/issues/136
func TestChannelCounterShouldNotPanicIssue136(t *testing.T) {
	if c := integrationConnection(t, "issue136"); c != nil {
		defer c.Close()
		var wg sync.WaitGroup

		// exceeds 65535 channels
		errs := make(chan error, 8*10000*2)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				for j := 0; j < 10000; j++ {
					ch, err := c.Channel()
					if err != nil {
						errs <- err
					}
					if err := ch.Close(); err != nil {
						errs <- err
					}
				}
				wg.Done()
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("failed to create or close channel, got: %v", err)
			}
		}
	}
}

func TestExchangeDeclarePrecondition(t *testing.T) {
	c1 := integrationConnection(t, "exchange-double-declare")
	c2 := integrationConnection(t, "exchange-double-declare-cleanup")
	if c1 != nil && c2 != nil {
		defer c1.Close()
		defer c2.Close()

		ch, err := c1.Channel()
		if err != nil {
			t.Fatalf("Create channel")
		}

		exchange := "test-mismatched-redeclare"

		err = ch.ExchangeDeclare(
			exchange,
			"direct", // exchangeType
			false,    // durable
			true,     // auto-delete
			false,    // internal
			false,    // noWait
			nil,      // arguments
		)
		if err != nil {
			t.Fatalf("Could not initially declare exchange")
		}

		err = ch.ExchangeDeclare(
			exchange,
			"direct",
			true, // different durability
			true,
			false,
			false,
			nil,
		)

		if err == nil {
			t.Fatalf("Expected to fail a redeclare with different durability, didn't receive an error")
		} else {
			declareErr := err.(*Error)
			if declareErr.Code != PreconditionFailed {
				t.Fatalf("Expected precondition error")
			}
			if !declareErr.Recover {
				t.Fatalf("Expected to be able to recover")
			}
		}

		ch2, _ := c2.Channel()
		if err = ch2.ExchangeDelete(exchange, false, false); err != nil {
			t.Fatalf("Could not delete exchange: %v", err)
		}
	}
}

func TestRabbitMQQueueTTLGet(t *testing.T) {
	if c := integrationRabbitMQ(t, "ttl"); c != nil {
		defer c.Close()

		queue := "test.rabbitmq-message-ttl"
		channel, err := c.Channel()
		if err != nil {
			t.Fatalf("channel: %v", err)
		}

		if _, err = channel.QueueDeclare(
			queue,
			false,
			true,
			false,
			false,
			Table{"x-message-ttl": int32(100)}, // in ms
		); err != nil {
			t.Fatalf("queue declare: %s", err)
		}

		if err := channel.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("ttl")}); err != nil {
			t.Fatalf("error publishing: %v", err)
		}

		time.Sleep(200 * time.Millisecond)

		_, ok, err := channel.Get(queue, false)

		if ok {
			t.Fatalf("Expected the message to expire in 100ms, it didn't expire after 200ms")
		}

		if err != nil {
			t.Fatalf("Failed to get on ttl queue")
		}
	}
}

func TestRabbitMQQueueNackMultipleRequeue(t *testing.T) {
	if c := integrationRabbitMQ(t, "nack"); c != nil {
		defer c.Close()

		if c.isCapable("basic.nack") {
			queue := "test.rabbitmq-basic-nack"
			channel, err := c.Channel()
			if err != nil {
				t.Fatalf("channel: %v", err)
			}

			if _, err = channel.QueueDeclare(queue, false, true, false, false, nil); err != nil {
				t.Fatalf("queue declare: %s", err)
			}

			if err := channel.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("1")}); err != nil {
				t.Fatalf("error publishing: %v", err)
			}
			if err := channel.PublishWithContext(context.TODO(), "", queue, false, false, Publishing{Body: []byte("2")}); err != nil {
				t.Fatalf("error publishing: %v", err)
			}

			m1, ok, err := channel.Get(queue, false)
			if !ok || err != nil || m1.Body[0] != '1' {
				t.Fatalf("could not get message %v", m1)
			}

			m2, ok, err := channel.Get(queue, false)
			if !ok || err != nil || m2.Body[0] != '2' {
				t.Fatalf("could not get message %v", m2)
			}

			if err := m2.Nack(true, true); err != nil {
				t.Fatalf("nack error: %v", err)
			}

			m1, ok, err = channel.Get(queue, false)
			if !ok || err != nil || m1.Body[0] != '1' {
				t.Fatalf("could not get message %v", m1)
			}

			m2, ok, err = channel.Get(queue, false)
			if !ok || err != nil || m2.Body[0] != '2' {
				t.Fatalf("could not get message %v", m2)
			}
		}
	}
}

func TestConsumerCancelNotification(t *testing.T) {
	c := integrationConnection(t, "consumer cancel notification")
	if c != nil {
		defer c.Close()
		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("got error on channel.open: %v", err)
		}

		queue := "test-consumer-cancel-notification"

		if _, err := ch.QueueDeclare(queue, false, true, false, false, nil); err != nil {
			t.Fatalf("expected to declare a queue: %v", err)
		}

		if _, err := ch.Consume(queue, "", false, false, false, false, nil); err != nil {
			t.Fatalf("basic.consume failed")
		}
		// consumer cancel notification channel
		ccnChan := make(chan string, 1)
		ch.NotifyCancel(ccnChan)

		if _, err := ch.QueueDelete(queue, false, false, true); err != nil {
			t.Fatalf("queue.delete failed: %s", err)
		}

		select {
		case <-ccnChan:
			// do nothing
		case <-time.After(time.Second * 10):
			t.Fatalf("basic.cancel wasn't received")
		}
		// we don't close ccnChan because channel shutdown
		// does it
	}
}

func TestConcurrentChannelAndConnectionClose(t *testing.T) {
	c := integrationConnection(t, "concurrent channel and connection test")
	if c != nil {
		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("got error on channel.open: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)

		starter := make(chan struct{})
		go func() {
			defer wg.Done()
			<-starter
			c.Close()
		}()

		go func() {
			defer wg.Done()
			<-starter
			ch.Close()
		}()
		close(starter)
		wg.Wait()
	}
}

func TestIntegrationGetNextPublishSeqNo(t *testing.T) {
	if c := integrationConnection(t, "GetNextPublishSeqNo"); c != nil {
		defer c.Close()

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("channel: %v", err)
		}

		if err = ch.Confirm(false); err != nil {
			t.Fatalf("could not confirm")
		}

		ex := "test-get-next-pub"
		if err = ch.ExchangeDeclare(ex, "direct", false, false, false, false, nil); err != nil {
			t.Fatalf("cannot declare %v: got: %v", ex, err)
		}

		n := ch.GetNextPublishSeqNo()
		if n != 1 {
			t.Fatalf("wrong next publish seqence number before any publish, expected: %d, got: %d", 1, n)
		}

		if err := ch.PublishWithContext(context.TODO(), "test-get-next-pub-seq", "", false, false, Publishing{}); err != nil {
			t.Fatalf("publish error: %v", err)
		}

		n = ch.GetNextPublishSeqNo()
		if n != 2 {
			t.Fatalf("wrong next publish seqence number after 1 publishing, expected: %d, got: %d", 2, n)
		}
	}
}

func TestIntegrationGetNextPublishSeqNoRace(t *testing.T) {
	if c := integrationConnection(t, "GetNextPublishSeqNoRace"); c != nil {
		defer c.Close()

		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("channel: %v", err)
		}

		if err = ch.Confirm(false); err != nil {
			t.Fatalf("could not confirm")
		}

		ex := "test-get-next-pub"
		if err = ch.ExchangeDeclare(ex, "direct", false, false, false, false, nil); err != nil {
			t.Fatalf("cannot declare %v: got: %v", ex, err)
		}

		n := ch.GetNextPublishSeqNo()
		if n != 1 {
			t.Fatalf("wrong next publish seqence number before any publish, expected: %d, got: %d", 1, n)
		}

		wg := sync.WaitGroup{}
		fail := false

		wg.Add(2)

		go func() {
			defer wg.Done()
			_ = ch.GetNextPublishSeqNo()
		}()

		go func() {
			defer wg.Done()
			if err := ch.PublishWithContext(context.TODO(), "test-get-next-pub-seq", "", false, false, Publishing{}); err != nil {
				t.Logf("publish error: %v", err)
				fail = true
			}
		}()

		wg.Wait()
		if fail {
			t.FailNow()
		}

		n = ch.GetNextPublishSeqNo()
		if n != 2 {
			t.Fatalf("wrong next publish seqence number after 15 publishing, expected: %d, got: %d", 2, n)
		}
	}
}

// https://github.com/rabbitmq/amqp091-go/pull/44
func TestShouldNotWaitAfterConnectionClosedIssue44(t *testing.T) {
	conn := integrationConnection(t, "TestShouldNotWaitAfterConnectionClosedIssue44")
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("channel error: %v", err)
	}
	err = ch.Confirm(false)
	if err != nil {
		t.Fatalf("confirm error: %v", err)
	}
	closed := conn.NotifyClose(make(chan *Error, 1))
	go func() {
		<-closed
	}()

	confirm, err := ch.PublishWithDeferredConfirmWithContext(context.TODO(), "test-issue44", "issue44", false, false, Publishing{Body: []byte("abc")})
	if err != nil {
		t.Fatalf("PublishWithDeferredConfirm error: %v", err)
	}

	ch.Close()
	conn.Close()

	ack := confirm.Wait()

	if ack != false {
		t.Fatalf("ack returned should be false %v", ack)
	}
}

func TestDeliveryAckShouldReturnSpecificErrorOnClosedChannel(t *testing.T) {
	// setup
	c, ch := integrationQueue(t, t.Name())
	defer c.Close()
	err := ch.Publish(DefaultExchange, t.Name(), false, false, Publishing{
		Body: []byte("this is a test"),
	})
	if err != nil {
		t.Fatalf("publish error: %v", err)
	}

	var ctx context.Context
	var cancel context.CancelFunc
	d, ok := t.Deadline()
	if !ok {
		ctx, cancel = context.WithTimeout(context.Background(), time.Second*10)
	} else {
		ctx, cancel = context.WithDeadline(context.Background(), d)
	}
	defer cancel()

	messages, err := ch.Consume(t.Name(), t.Name(), false, false, false, false, Table{})
	if err != nil {
		t.Fatalf("consume error: %v", err)
	}

	// Act
	<-time.After(time.Second) //😮‍💨
	err = ch.Close()
	if err != nil {
		t.Fatalf("close error: %v", err)
	}

	// Assert
	select {
	case <-messages:
		// ok, drain the channel
		println("received message")
	case <-ctx.Done():
		t.Fatalf("timed out waiting to receive a message or an error: %v", ctx.Err())
	}

	select {
	case msg, ok := <-messages:
		if ok {
			t.Fatal("message was not closed")
		}
		err := msg.Ack(false)
		if err == nil {
			t.Fatal("ack should have failed")
		}
		if !errors.Is(err, ErrDeliveryNotInitialized) {
			t.Fatalf("expected error '%s', got '%s'", ErrDeliveryNotInitialized.Error(), err.Error())
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting to receive a message or an error: %v", ctx.Err())
	}
}

// https://github.com/rabbitmq/amqp091-go/issues/11
func TestShouldNotWaitAfterConnectionClosedNewChannelCreatedIssue11(t *testing.T) {
	conn := integrationConnection(t, "TestShouldNotWaitAfterConnectionClosedNewChannelCreatedIssue11")
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("channel error: %v", err)
	}

	conn.NotifyClose(make(chan *Error, 1))

	_, err = ch.PublishWithDeferredConfirmWithContext(context.TODO(), "issue11", "issue11", false, false, Publishing{Body: []byte("abc")})
	if err != nil {
		t.Fatalf("PublishWithDeferredConfirm error: %v", err)
	}

	ch.Close()
	conn.Close()

	_, err = conn.Channel()
	if err == nil {
		t.Fatalf("Opening a channel from a closed connection should not block but returning an error %v", err)
	}
}

// https://github.com/rabbitmq/amqp091-go/issues/296
func TestAckShouldNotCloseChannel_GH296(t *testing.T) {
	// setup
	const messageCount = 10
	queueName := t.Name()
	c, ch := integrationQueue(t, queueName)
	defer ch.Close()
	defer c.Close()

	notifyConnClosed := make(chan *Error)
	c.NotifyClose(notifyConnClosed)

	notifyChannelClosed := make(chan *Error)
	ch.NotifyClose(notifyChannelClosed)

	for i := 0; i < messageCount; i++ {
		err := ch.Publish(DefaultExchange, queueName, false, false, Publishing{
			Body: []byte("this is a test"),
		})
		if err != nil {
			t.Fatalf("publish error: %v", err)
		}
	}

	err := ch.Qos(3, 0, false)
	if err != nil {
		t.Fatalf("Qos error: %v", err)
	}

	msgs, err := ch.Consume(
		queueName, // queue
		t.Name(),  // consumer
		false,     // auto-ack
		false,     // exclusive
		false,     // no-local
		false,     // no-wait
		nil,       // args
	)
	if err != nil {
		t.Fatalf("Consume error: %v", err)
	}

	signal := make(chan int)
	acks := make(chan *Delivery, messageCount)

	go func() {
		counter := 0
		for {
			select {
			case msg := <-msgs:
				go worker(acks, &msg)
			case ack := <-acks:
				ackError := ack.Ack(false)
				if ackError != nil {
					t.Logf("Ack error: %v", ackError)
				}
				counter = counter + 1
				if counter >= messageCount {
					signal <- counter
					return
				}
			case <-time.After(500 * time.Millisecond):
				t.Log("timed out waiting to do something")
			}
		}
	}()

	select {
	case connError := <-notifyConnClosed:
		t.Logf("saw connection closure error: %v", connError)
	case channelError := <-notifyChannelClosed:
		t.Logf("saw channel closure error: %v", channelError)
	case count := <-signal:
		t.Logf("saw %d messages", count)
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting to see %d messages", messageCount)
	}
}

func worker(acks chan<- *Delivery, msg *Delivery) {
	time.Sleep(time.Millisecond * time.Duration(msg.DeliveryTag) * 100)
	acks <- msg
}

/*
 * Support for integration tests
 */

// Returns a connection to the AMQP if the AMQP_URL environment
// variable is set and a connection can be established.
func integrationConnection(t *testing.T, name string) *Connection {
	t.Helper()

	conf := defaultConfig()
	if conf.Properties == nil {
		conf.Properties = make(Table)
	}
	conf.Properties.SetClientConnectionName(name)
	conn, err := DialConfig(amqpURL, conf)
	if err != nil {
		t.Fatalf("cannot dial integration server. Is the rabbitmq-server service running? %s", err)
		return nil
	}
	return conn
}

// Returns a connection, channel and declares a queue when the AMQP_URL is in the environment
func integrationQueue(t *testing.T, name string) (*Connection, *Channel) {
	t.Helper()

	if conn := integrationConnection(t, name); conn != nil {
		if channel, err := conn.Channel(); err == nil {
			if _, err = channel.QueueDeclare(name, false, true, false, false, nil); err == nil {
				return conn, channel
			}
		}
	}
	return nil, nil
}

func integrationQueueDelete(t *testing.T, c *Channel, queue string) {
	t.Helper()

	if c, err := c.QueueDelete(queue, false, false, false); err != nil {
		t.Fatalf("queue deletion failed, c: %d, err: %v", c, err)
	}
}

// Delegates to integrationConnection and only returns a connection if the
// product is RabbitMQ
func integrationRabbitMQ(t *testing.T, name string) *Connection {
	t.Helper()

	if conn := integrationConnection(t, name); conn != nil {
		if server, ok := conn.Properties["product"]; ok && server == "RabbitMQ" {
			return conn
		}
	}

	return nil
}

func assertConsumeBody(t *testing.T, messages <-chan Delivery, want []byte) (msg *Delivery) {
	t.Helper()

	select {
	case got := <-messages:
		if !bytes.Equal(want, got.Body) {
			t.Fatalf("Message body does not match want: %v, got: %v, for: %+v", want, got.Body, got)
		}
		msg = &got
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("Timeout waiting for %v", want)
	}

	return msg
}

// Pulls out the CRC and verifies the remaining content against the CRC
func assertMessageCrc32(t *testing.T, msg []byte, assert string) {
	t.Helper()

	size := binary.BigEndian.Uint32(msg[:4])

	crc := crc32.NewIEEE()
	crc.Write(msg[8:])

	if binary.BigEndian.Uint32(msg[4:8]) != crc.Sum32() {
		t.Fatalf("Message does not match CRC: %s", assert)
	}

	if int(size) != len(msg)-8 {
		t.Fatalf("Message does not match size, should=%d, is=%d: %s", size, len(msg)-8, assert)
	}
}

// Creates a random body size with a leading 32-bit CRC in network byte order
// that verifies the remaining slice
func generateCrc32Random(t *testing.T, size int) []byte {
	t.Helper()

	msg := make([]byte, size+8)
	if _, err := io.ReadFull(devrand.Reader, msg); err != nil {
		t.Fatalf("could not get random data: %+v", err)
	}

	crc := crc32.NewIEEE()
	crc.Write(msg[8:])

	binary.BigEndian.PutUint32(msg[0:4], uint32(size))
	binary.BigEndian.PutUint32(msg[4:8], crc.Sum32())

	return msg
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

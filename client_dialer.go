package remotedialer

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

func clientDial(dialer Dialer, conn *connection, message *message) {
	fmt.Println("1--------clientDial start")
	defer conn.Close()

	var (
		netConn net.Conn
		err     error
	)

	if dialer == nil {
		netConn, err = net.DialTimeout(message.proto, message.address, time.Duration(message.deadline)*time.Millisecond)
	} else {
		netConn, err = dialer(message.proto, message.address)
	}
	fmt.Println("1--------clientDial time deadline", message.deadline)
	// netConn.SetDeadline(time.Now().Add(time.Duration(message.deadline) * time.Millisecond))

	if err != nil {
		conn.tunnelClose(err)
		return
	}
	defer netConn.Close()

	pipe(conn, netConn)
	fmt.Println("1--------clientDial end")
}

func pipe(client *connection, server net.Conn) {
	fmt.Println("1--------pipe start")
	wg := sync.WaitGroup{}
	wg.Add(1)

	close := func(err error) error {
		if err == nil {
			err = io.EOF
		}
		client.doTunnelClose(err)
		server.Close()
		return err
	}

	go func() {
		defer wg.Done()
		fmt.Println("1--------pipe client start")
		_, err := io.Copy(server, client)
		fmt.Println("2--------pipe client close")
		close(err)
	}()

	fmt.Println("1--------pipe server start")
	_, err := io.Copy(client, server)
	fmt.Println("2--------pipe server close")
	err = close(err)
	wg.Wait()

	// Write tunnel error after no more I/O is happening, just incase messages get out of order
	client.writeErr(err)
	fmt.Println("1--------pipe end")
}

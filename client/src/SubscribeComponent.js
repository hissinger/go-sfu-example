import React, { useEffect, useState, useRef } from "react";
import Peer from "simple-peer";

const RoomComponent = (props) => {
  const [wsConn, setWsConn] = useState();
  const userVideoRef = useRef();

  useEffect(() => {
    const c = new WebSocket("ws://localhost:8080/ws");
    setWsConn(c);
    c.onopen = function () {
      console.log("connected");
    };
  }, []);

  function handleClick() {
    const peer = new Peer({
      initiator: true,
      trickle: true,
      config: {
        iceServers: [
          {
            url: "stun:stun.l.google.com:19302",
          },
        ],
      },
    });

    peer.on("error", (err) => console.log("error", err));
    peer.on("signal", (data) => {
      console.log(JSON.stringify(data));
      wsConn.send(JSON.stringify(data));
    });
    peer.on("stream", (stream) => {
      if (userVideoRef.current) {
        userVideoRef.current.srcObject = stream;
      }
    });

    wsConn.onmessage = function (message) {
      console.log(message.data);
      peer.signal(message.data);
    };
  }

  return (
    <div>
      <video playsInline muted ref={userVideoRef} autoPlay />
      <button onClick={handleClick}>SUBSCRIBE</button>
    </div>
  );
};

export default RoomComponent;

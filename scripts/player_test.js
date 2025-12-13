// player_test.js
const WebSocket = require('ws');

const token = 'player123token';
const ws = new WebSocket(`ws://localhost:8888/ws?token=${token}`);

ws.on('open', () => {
    console.log('Connected as player!');
});

ws.on('message', (data) => {
    console.log('Received:', data.toString());
});

ws.on('close', () => {
    console.log('Connection closed');
});

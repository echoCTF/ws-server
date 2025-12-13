// server_publish.js
// curl -X POST http://localhost:8888/publish \
//  -H "Authorization: Bearer server123token" \
//  -H "Content-Type: application/json" \
//  -d '{"player_id":"player1","event":"score_update","payload":{"score":42}}'
const fetch = require('node-fetch');

const token = 'server123token';
const payload = {
    player_id: 'player1',
    event: 'score_update',
    payload: { score: Math.floor(Math.random() * 100) }
};

fetch('http://localhost:8888/publish', {
    method: 'POST',
    headers: {
        'Authorization': `Bearer ${token}`,
        'Content-Type': 'application/json'
    },
    body: JSON.stringify(payload)
})
.then(res => {
    console.log('Status:', res.status);
    return res.text();
})
.then(text => console.log(text))
.catch(err => console.error(err));

// server_publish.js
// curl -X POST http://localhost:8888/publish \
//  -H "Authorization: Bearer server123token" \
//  -H "Content-Type: application/json" \
//  -d '{"player_id":"player1","event":"score_update","payload":{"score":42}}'

//curl -X POST http://localhost:8888/broadcast \
//  -H "Authorization: Bearer server123token" \
//  -H "Content-Type: application/json" \
//  -d '{
//    "event": "apiNotifications",
//    "payload": {
//      "message": "Server restart in 5 minutes"
//    }
//  }'
const fetch = require('node-fetch');

const token = 'server123token';
const notificationPayload = {
  player_id: '21',
  event: 'notification',
  payload: {
    type: 'success',
    title: 'Hey you',
    body: '<p>Hey you guy</p>'
   }
};
const targetPayload = {
  player_id: '21',
  event: 'target',
  payload: {
    type: 'update',
    id: 13,
   }
};

fetch('http://localhost:8888/publish', {
  method: 'POST',
  headers: {
    'Authorization': `Bearer ${token}`,
    'Content-Type': 'application/json'
  },
  body: JSON.stringify(notificationPayload)
})
  .then(res => {
    console.log('Status:', res.status);
    return res.text();
  })
  .then(text => console.log(text))
  .catch(err => console.error(err));


fetch('http://localhost:8888/publish', {
  method: 'POST',
  headers: {
    'Authorization': `Bearer ${token}`,
    'Content-Type': 'application/json'
  },
  body: JSON.stringify(targetPayload)
})
  .then(res => {
    console.log('Status:', res.status);
    return res.text();
  })
  .then(text => console.log(text))
  .catch(err => console.error(err));

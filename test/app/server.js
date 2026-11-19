const greet = require('./lib/greet')
const leftpad = require('leftpad')
const data = require('./data.json')
const fs = require('fs')
const path = require('path')
const { Buffer } = require('buffer')
const url = require('url')
const qs = require('querystring')
const util = require('util')
const EventEmitter = require('events')

const BASE = 'http://127.0.0.1'

app.use((req, res, next) => {
  res.set('X-Global', 'yes')
  next()
})

app.get('/', (req, res) => {
  res.json({ ok: true, path: req.path, method: req.method })
})

app.get('/hello', (req, res) => {
  res.send('hello world')
})

app.get('/version', (req, res) => {
  res.send('v1')
})

app.post('/echo', (req, res) => {
  res.status(201).json(req.body)
})

app.get('/query', (req, res) => {
  res.json(req.query)
})

app.get('/queryall', (req, res) => {
  res.json(req.queryAll)
})

app.get('/header', (req, res) => {
  res.set('X-Custom', 'yes').send('ok')
})

app.get('/type', (req, res) => {
  res.type('text/csv').send('a,b')
})

app.get('/redirect', (req, res) => {
  res.redirect(302, '/hello')
})

app.get('/end', (req, res) => {
  res.status(204).end()
})

app.get('/append', (req, res) => {
  res.append('X-Multi', 'one').append('X-Multi', ['two', 'three']).send('ok')
})

app.get('/location', (req, res) => {
  res.location('/hello').status(201).send('ok')
})

app.get('/location-back', (req, res) => {
  res.location('back').status(303).send('ok')
})

app.get('/cookie', (req, res) => {
  res.cookie('sid', 'a b', {
    path: '/app',
    maxAge: 60000,
    httpOnly: true,
    sameSite: 'lax'
  }).send('ok')
})

app.get('/sendstatus', (req, res) => {
  res.sendStatus(418)
})

app.get('/flush', (req, res) => {
  res.type('text/plain').flush().send('flushed')
})

app.get('/sendfile', (req, res) => {
  res.sendFile('hello.txt', {
    root: 'public',
    headers: { 'X-File': 'yes' },
    maxAge: 60000
  })
})

app.head('/sendfile-head', (req, res) => {
  res.sendFile('hello.txt', { root: 'public' })
})

app.get('/sendfile-missing', (req, res) => {
  res.sendFile('nope.txt', { root: 'public' })
})

app.get('/sendfile-dotfile', (req, res) => {
  res.sendFile('.secret', { root: 'public', dotfiles: 'deny' })
})

app.get('/sendfile-escape', (req, res) => {
  res.sendFile('../server.js', { root: 'public' })
})

app.post('/form', (req, res) => {
  res.json(req.body)
})

app.get('/echoheader', (req, res) => {
  res.send(req.header('X-Test'))
})

app.get('/request', (req, res) => {
  res.json({
    url: req.url,
    originalUrl: req.originalUrl,
    ip: req.ip,
    protocol: req.protocol,
    secure: req.secure,
    viaGet: req.get('X-Test') || null
  })
})

app.get('/env', (req, res) => {
  res.json({
    secret: process.env.E2E_SECRET || null,
    home: process.env.HOME || null
  })
})

app.get('/fetch-allowed', (req, res) => {
  fetch(BASE + '/hello', (err, upstream) => {
    if (err) return res.status(502).send(String(err))
    res.json({ status: upstream.status, text: upstream.text })
  })
})

app.get('/fetch-blocked', (req, res) => {
  fetch('http://example.com/', (err) => {
    if (err) return res.status(403).send('blocked')
    res.status(200).send('allowed')
  })
})

app.get('/fetch/promise', (req, res) => {
  fetch(BASE + '/hello')
    .then((upstream) => upstream.text())
    .then((text) => res.json({ status: 200, text }))
    .catch((err) => res.status(502).send(err.message))
})

app.get('/fetch/headers', (req, res) => {
  fetch(BASE + '/header')
    .then((upstream) => res.json({
      status: upstream.status,
      ok: upstream.ok,
      custom: upstream.headers.get('X-Custom')
    }))
    .catch((err) => res.status(502).send(err.message))
})

app.get('/fetch/post', (req, res) => {
  fetch(BASE + '/echo', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ a: 1 })
  })
    .then((upstream) => upstream.json())
    .then((echoed) => res.json({ status: 201, echoed }))
    .catch((err) => res.status(502).send(err.message))
})

app.get('/fetch/stream', (req, res) => {
  fetch(BASE + '/hello')
    .then((upstream) => {
      const reader = upstream.body.getReader()
      let out = ''
      function pump() {
        return reader.read().then((step) => {
          if (step.done) return out
          out += step.value
          return pump()
        })
      }
      return pump()
    })
    .then((text) => res.send(text))
    .catch((err) => res.status(502).send(err.message))
})

app.get('/fetch/reject', (req, res) => {
  fetch('http://example.com/')
    .then(() => res.send('allowed'))
    .catch((err) => res.status(502).send(err.message))
})

const express = require('express')
const api = express()

api.get('/express/hello', (req, res) => res.send('express hello'))
api.put('/express/put', (req, res) => res.send('put ok'))
api.patch('/express/patch', (req, res) => res.send('patch ok'))
api.delete('/express/delete', (req, res) => res.send('delete ok'))
api.all('/express/any', (req, res) => res.send(req.method))
api.options('/express/options', (req, res) => res.set('Allow', 'GET, PUT, DELETE').send(''))
api.head('/express/head', (req, res) => {
  res.set('X-Head', 'yes').send('')
})
api.listen(3000)

const router = express.Router()
router.get('/hello', (req, res) => res.send('router hello'))
router.get('/user/:id', (req, res) => res.json({ id: req.params.id }))
router.route('/chain/:id')
  .get((req, res) => res.json({ id: req.params.id }))
  .post((req, res) => res.status(201).json({ created: req.params.id }))
app.use('/api', router)

const paramRouter = express.Router()
paramRouter.param('id', (req, res, next, value, name) => {
  req.params['seen_' + name] = value
  next()
})
paramRouter.get('/item/:id', (req, res) => {
  res.json({ id: req.params.id, seen: req.params.seen_id })
})
app.use('/param', paramRouter)

app.use('/static', express.static('public'))

app.use('/mw', (req, res, next) => {
  res.set('X-MW', 'yes')
  next()
})

app.get('/mw/hello', (req, res) => res.send('mw hello'))

app.get(
  '/mw/route',
  (req, res, next) => {
    res.set('X-Route-MW', '1')
    next()
  },
  (req, res) => res.send('route mw')
)

app.get('/users/:id', (req, res) => res.json({ id: req.params.id }))
app.get('/users/:id/posts/:post', (req, res) => res.json(req.params))
app.get('/files/*', (req, res) => res.send(String(req.params[0])))

app.get('/boom', (req, res, next) => next(new Error('kaboom')))
app.use('/boom', (err, req, res, next) => {
  res.status(503).send('handled: ' + err.message)
})

app.get('/throw', (req, res) => {
  throw new Error('nope')
})

app.get('/module/relative', (req, res) => res.send(greet('world')))
app.get('/module/node-modules', (req, res) => res.send(leftpad('7')))
app.get('/module/json', (req, res) => res.json(data))

app.get('/fs/read', (req, res) => {
  fs.readFile(path.join('public', 'hello.txt'), (err, text) => {
    if (err) return res.status(500).send(String(err))
    res.send(text)
  })
})

app.get('/fs/write', (req, res) => {
  fs.writeFile('tmp/out.txt', 'written', (err) => {
    if (err) return res.status(500).send(String(err))
    res.send('ok')
  })
})

app.get('/fs/read-written', (req, res) => {
  try {
    res.send(fs.readFileSync('tmp/out.txt'))
  } catch (err) {
    res.status(500).send(String(err))
  }
})

app.get('/fs/absolute', (req, res) => {
  try {
    res.send(fs.readFileSync('/public/hello.txt'))
  } catch (err) {
    res.status(500).send(String(err))
  }
})

app.get('/fs/clamp', (req, res) => {
  try {
    res.send(fs.readFileSync('../public/hello.txt'))
  } catch (err) {
    res.status(500).send(String(err))
  }
})

app.get('/fs/missing', (req, res) => {
  fs.readFile('tmp/nope.txt', (err) => {
    if (err) return res.status(500).send(String(err))
    res.send('unexpected')
  })
})

app.get('/path/join', (req, res) => {
  res.send(path.join('a', 'b', '..', 'c'))
})

app.get('/shims', (req, res) => {
  const buf = Buffer.from('6869', 'hex')
  const u = url.parse('http://example.com/a?b=c')
  const q = qs.stringify({ x: '1', y: '2' })
  const f = util.format('%s-%d', 'v', 2)
  const e = new EventEmitter()
  let seen = ''
  e.on('ping', (v) => { seen += v })
  e.emit('ping', 'ok')
  res.json({
    buffer: buf.toString(),
    host: u.hostname,
    query: q,
    format: f,
    events: seen
  })
})

app.get('/timer', (req, res) => {
  setTimeout(() => res.send('timer fired'), 10)
})

app.get('/interval', (req, res) => {
  let ticks = 0
  const id = setInterval(() => {
    ticks++
    if (ticks === 2) {
      clearInterval(id)
      res.send('interval ' + ticks)
    }
  }, 10)
})

app.get('/timer/spin', (req, res) => {
  setTimeout(() => {
    while (true) {}
  }, 10)
})

app.use((req, res) => {
  res.status(404).send('not found')
})

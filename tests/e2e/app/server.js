const greet = require('./lib/greet')
const leftpad = require('leftpad')
const data = require('./data.json')

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

app.post('/form', (req, res) => {
  res.json(req.body)
})

app.get('/echoheader', (req, res) => {
  res.send(req.header('X-Test'))
})

app.get('/env', (req, res) => {
  res.json({
    secret: process.env.E2E_SECRET || null,
    home: process.env.HOME || null
  })
})

app.get('/fetch-allowed', (req, res) => {
  fetch('http://' + req.host + '/hello', (err, upstream) => {
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
  fetch('http://' + req.host + '/hello')
    .then((upstream) => upstream.text())
    .then((text) => res.json({ status: 200, text }))
    .catch((err) => res.status(502).send(err.message))
})

app.get('/fetch/headers', (req, res) => {
  fetch('http://' + req.host + '/header')
    .then((upstream) => res.json({
      status: upstream.status,
      ok: upstream.ok,
      custom: upstream.headers.get('X-Custom')
    }))
    .catch((err) => res.status(502).send(err.message))
})

app.get('/fetch/post', (req, res) => {
  fetch('http://' + req.host + '/echo', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ a: 1 })
  })
    .then((upstream) => upstream.json())
    .then((echoed) => res.json({ status: 201, echoed }))
    .catch((err) => res.status(502).send(err.message))
})

app.get('/fetch/stream', (req, res) => {
  fetch('http://' + req.host + '/hello')
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
api.delete('/express/delete', (req, res) => res.send('delete ok'))
api.all('/express/any', (req, res) => res.send(req.method))
api.head('/express/head', (req, res) => {
  res.set('X-Head', 'yes').send('')
})
api.listen(3000)

const router = express.Router()
router.get('/hello', (req, res) => res.send('router hello'))
router.get('/user/:id', (req, res) => res.json({ id: req.params.id }))
app.use('/api', router)

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

app.use((req, res) => {
  res.status(404).send('not found')
})

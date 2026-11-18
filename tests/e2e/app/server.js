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

app.use((req, res) => {
  res.status(404).send('not found')
})

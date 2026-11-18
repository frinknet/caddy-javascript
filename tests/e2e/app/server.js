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

app.use((req, res) => {
  res.status(404).send('not found')
})

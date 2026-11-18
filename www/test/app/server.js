app.get('/', (req, res) => {
  res.json({ ok: true, path: req.path })
})

app.post('/echo', (req, res) => {
  res.status(201).json(req.body)
})

app.use((req, res) => {
  res.status(404).send('not found')
})

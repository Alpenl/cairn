import { describe, expect, it } from 'vitest'
import { discoverFeeds } from './rss-discovery'

describe('discoverFeeds', () => {
  it('discovers RSS, Atom and RDF declarations, resolves relative URLs and deduplicates', () => {
    document.documentElement.innerHTML = `
      <head>
        <title>Example Site</title>
        <link rel="alternate" type="application/rss+xml" title="Posts" href="/feed.xml#top">
        <link rel="ALTERNATE stylesheet" type="application/rss+xml" title="Duplicate" href="https://example.com/feed.xml">
        <link rel="alternate" type="application/atom+xml" title="Atom" href="atom.xml">
        <link rel="alternate" type="application/rdf+xml" title="RDF" href="/index.rdf">
        <link rel="alternate" type="text/html" href="/not-a-feed">
      </head>
      <body></body>
    `

    expect(discoverFeeds(document, 'https://example.com/blog/')).toEqual([
      {
        title: 'Posts',
        url: 'https://example.com/feed.xml',
        format: 'rss',
      },
      {
        title: 'Atom',
        url: 'https://example.com/blog/atom.xml',
        format: 'atom',
      },
      {
        title: 'RDF',
        url: 'https://example.com/index.rdf',
        format: 'rdf',
      },
    ])
  })

  it('falls back to the page title and ignores unsafe URLs', () => {
    document.documentElement.innerHTML = `
      <head>
        <title>  My   Notes  </title>
        <link rel="alternate" type="application/rss+xml" href="javascript:alert(1)">
        <link rel="alternate" type="application/atom+xml" href="/atom.xml">
      </head>
    `

    expect(discoverFeeds(document, 'https://notes.test/page')).toEqual([
      {
        title: 'My Notes',
        url: 'https://notes.test/atom.xml',
        format: 'atom',
      },
    ])
    expect(discoverFeeds(document, 'chrome://settings')).toEqual([])
  })

  it.each([
    [
      'RSS',
      '<rss version="2.0"><channel><title>RSS Direct</title></channel></rss>',
      'rss',
      'RSS Direct',
    ],
    [
      'Atom',
      '<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom Direct</title></feed>',
      'atom',
      'Atom Direct',
    ],
    [
      'RDF',
      '<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><channel><title>RDF Direct</title></channel></rdf:RDF>',
      'rdf',
      'RDF Direct',
    ],
  ])(
    'recognizes a directly opened %s XML document',
    (_name, xml, format, title) => {
      const xmlDocument = new DOMParser().parseFromString(
        xml,
        'application/xml',
      )
      expect(
        discoverFeeds(xmlDocument, 'https://feeds.test/current.xml'),
      ).toEqual([
        {
          title,
          url: 'https://feeds.test/current.xml',
          format,
        },
      ])
    },
  )
})

import {
  installReaderNavigationGuard,
  notifyReaderNavigationCommitted,
  readerHistoryState,
} from '../src/lib/navigation/route'

const app = document.createElement('main')
const surface = document.createElement('output')
surface.dataset.testid = 'surface'
const draft = document.createElement('output')
draft.dataset.testid = 'draft'
const dirtyButton = document.createElement('button')
dirtyButton.type = 'button'
dirtyButton.textContent = 'make-dirty'
const historyButton = document.createElement('button')
historyButton.type = 'button'
historyButton.textContent = 'commit-history'
app.append(surface, draft, dirtyButton, historyButton)
document.body.append(app)

let dirty = false
let currentSurface = 'reading'

function render(): void {
  surface.textContent = currentSurface
  draft.textContent = dirty ? 'dirty' : 'clean'
}

function updateSurfaceFromLocation(): void {
  currentSurface = new URL(window.location.href).searchParams.get('tool') === 'history'
    ? 'history'
    : 'reading'
  render()
}

window.history.replaceState(readerHistoryState(window.history.state, 0), '', '?view=reading')
installReaderNavigationGuard(() => !dirty || window.confirm('discard dirty draft?'))
window.addEventListener('popstate', updateSurfaceFromLocation)

dirtyButton.addEventListener('click', () => {
  dirty = true
  render()
})

historyButton.addEventListener('click', () => {
  window.history.pushState(readerHistoryState(window.history.state, 1), '', '?tool=history')
  currentSurface = 'history'
  notifyReaderNavigationCommitted()
  render()
})

render()

import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// jsdom has no canvas: xterm.js asks for a 2D context when it loads, and jsdom would print "Not implemented" for it.
// Answering "no context" is what a browser without canvas does, and is all the tests need.
HTMLCanvasElement.prototype.getContext = () => null

afterEach(() => {
  cleanup()
})

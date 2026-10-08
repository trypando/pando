import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import './design/styles.css';
import './ui/narrow.css';
import { App } from './app/App';

// Server state lives in the query cache; there is no client store. Design 08
// §1.2 allows Zustand "sparingly" and so far nothing needs it — every screen
// renders what an endpoint returned.
const queries = new QueryClient({
  defaultOptions: {
    queries: {
      // The reconciler converges on a 15s tick, so anything faster shows churn
      // the user cannot act on.
      staleTime: 10_000,
      retry: 1,
      // Off, and turned on where coming back to the tab should show something
      // new: who is signed in, the inbox count, an app's status, the first
      // page of approvals and of an app's events (issue #72). On by default it
      // read every query on the screen again — a dozen on an app's screen —
      // each time the window took focus, in every open console. A screen
      // still reads its queries again when it is opened, once they are older
      // than staleTime.
      refetchOnWindowFocus: false,
    },
  },
});

const root = document.getElementById('root');
if (!root) throw new Error('#root is missing from index.html');

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queries}>
      <App />
    </QueryClientProvider>
  </StrictMode>,
);

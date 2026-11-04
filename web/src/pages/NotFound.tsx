import { Link } from "react-router";

export function NotFound() {
  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-4 px-5 py-8 md:px-10 md:py-12">
      <title>Page not found – gedour</title>
      <h1 className="font-display text-3xl font-semibold tracking-tight">Page not found</h1>
      <p className="text-muted">This address does not match any page.</p>
      <Link to="/inbox" className="w-fit font-medium text-ink underline underline-offset-4">
        Go to the inbox
      </Link>
    </div>
  );
}

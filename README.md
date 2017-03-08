This is a partial implementation of a FastCGI server. The server offers only
the Responder role.

I use it for debugging web servers that connect to FastCGI servers. It is not
useful beyond debugging, mainly because I've hardcoded its response (beyond what
the command line arguments offer).

While the Go standard library includes a FastCGI package, I made this in order
to gain deep control over the protocol, as well as to help understand it.

dist.bat packages the Go toolchain for Windows in both zip
and installer (msi) format.

Dependencies
============
- Windows Installer XML (WiX) toolset: http://wix.sourceforge.net/
- 7Zip (command line version): http://www.7-zip.org/download.html
- Mercurial (hg): http://mercurial.selenic.com/


Packaging
=========
The dependencies must be callable from dist.bat, therefore,
they'll need to be in/added to the system's search PATH. 

The packaging needs to be done from within a tracked Go folder. 
Packages are built by cloning the same version of the source tree
that the Go tools were built from.

Run dist.bat from a command prompt or click on the batch file.

TODO
----
- Write a Go program for dist.bat functionality
- Documentation server shortcut checkbox option

Misc
----
WiX box sizes:
 - banner size: 493x58
 - left side of dialog: 164x312
 - full dialog size: 493x312



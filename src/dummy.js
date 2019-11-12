import React, { useEffect } from "react";

const Dummy = ({ title, text }) => {
  useEffect(() => {}, [
    document.title = title
  ]);
  return <div>{text}</div>;
};

export default Dummy;
